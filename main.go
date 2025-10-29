package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/abema/go-mp4"
	"github.com/chai2010/webp"
	"github.com/disintegration/imaging"
	"github.com/joho/godotenv"
	"github.com/schollz/progressbar/v3"
	flag "github.com/spf13/pflag"
)

const (
	defaultBaseURL = "https://api.openai.com/v1"

	ffmpegInstallMsg = `ffmpeg is required but was not found in PATH.
Please install ffmpeg:
  Ubuntu/Debian: sudo apt-get install ffmpeg
  macOS: brew install ffmpeg
  Or download from: https://ffmpeg.org/download.html`
)

type remixVideoRequest struct {
	Prompt string `json:"prompt"`
}

type createVideoResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// Some APIs may return error directly
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
}

type videoStatusResponse struct {
	ID       string    `json:"id"`
	Status   string    `json:"status"`
	Error    *apiError `json:"error,omitempty"`
	Progress int       `json:"progress,omitempty"` // 0-100 percentage
}

type videoHistoryEntry struct {
	ID          string  `json:"id"`
	Prompt      string  `json:"prompt"`
	CreatedAt   string  `json:"created_at"`
	OutputFile  string  `json:"output_file,omitempty"`
	Model       string  `json:"model"`
	ImageInput  *string `json:"image_input,omitempty"`
	RemixedFrom *string `json:"remixed_from,omitempty"`
}

type history struct {
	Videos []videoHistoryEntry `json:"videos"`
}

func main() {
	var (
		prompt      string
		output      string
		usePro      bool
		baseURL     string
		inputFile   string
		remixFrom   string
		listHistory bool
		seconds     string
		portrait    bool
		landscape   bool
	)

	flag.StringVarP(&prompt, "prompt", "p", "", "Text prompt for the video. If empty, reads interactively.")
	flag.StringVarP(&output, "output", "o", "", "Write output to <file>. Use '-' for stdout-only (no save). Default saves to {video_id}.mp4")
	flag.StringVar(&inputFile, "file", "", "Path to input image or video file (for image-to-video or video-to-video generation)")
	flag.StringVar(&remixFrom, "remix", "", "Remix from previous Sora video (@last, @0, @1, or video_id)")
	flag.BoolVar(&listHistory, "list", false, "List generation history and exit")
	flag.BoolVar(&usePro, "pro", false, "Use sora-2-pro model (better quality at same 720p resolution, 3x cost)")
	flag.StringVar(&seconds, "seconds", "8", "Video duration in seconds: 4, 8, or 12")
	flag.BoolVar(&portrait, "portrait", false, "Generate portrait video (720x1280)")
	flag.BoolVar(&landscape, "landscape", false, "Generate landscape video (1280x720, default)")
	flag.StringVar(&baseURL, "base-url", defaultBaseURL, "OpenAI API base URL")
	flag.Parse()

	// Validate remix conflicts - these flags don't apply when remixing
	if remixFrom != "" {
		conflicts := []struct {
			flag string
			set  bool
		}{
			{"--pro", usePro},
			{"--portrait", portrait},
			{"--landscape", landscape},
			{"--seconds", flag.Lookup("seconds").Changed},
		}

		var conflictNames []string
		for _, c := range conflicts {
			if c.set {
				conflictNames = append(conflictNames, c.flag)
			}
		}

		if len(conflictNames) > 0 {
			fmt.Fprintf(os.Stderr, "Error: Cannot use %s with --remix\n", strings.Join(conflictNames, ", "))
			fmt.Fprintln(os.Stderr, "When remixing, duration, resolution, and model are inherited from the original video.")
			fmt.Fprintln(os.Stderr, "To transform a video with different parameters, use --file instead.")
			os.Exit(2)
		}
	}

	// Validate seconds
	if seconds != "4" && seconds != "8" && seconds != "12" {
		fmt.Fprintf(os.Stderr, "Invalid --seconds value: %s (must be 4, 8, or 12)\n", seconds)
		os.Exit(2)
	}

	// Determine model based on --pro flag
	model := "sora-2"
	if usePro {
		model = "sora-2-pro"
	}

	// Determine video size
	if portrait && landscape {
		fmt.Fprintln(os.Stderr, "Cannot use both --portrait and --landscape")
		os.Exit(2)
	}
	var videoSize string
	if portrait {
		videoSize = "720x1280"
	} else {
		// Default to landscape 720p
		videoSize = "1280x720"
	}

	// Handle --list command
	if listHistory {
		h, err := loadHistory()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to load history: %v\n", err)
			os.Exit(1)
		}
		if len(h.Videos) == 0 {
			fmt.Fprintln(os.Stderr, "No videos in history")
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "Video Generation History:\n")
		for i, v := range h.Videos {
			fmt.Fprintf(os.Stderr, "[%d] %s\n", i, v.ID)
			fmt.Fprintf(os.Stderr, "    Created: %s\n", v.CreatedAt)
			fmt.Fprintf(os.Stderr, "    Model:   %s\n", v.Model)
			fmt.Fprintf(os.Stderr, "    Prompt:  %s\n", v.Prompt)
			if v.OutputFile != "" {
				fmt.Fprintf(os.Stderr, "    Output:  %s\n", v.OutputFile)
			}
			if v.ImageInput != nil && *v.ImageInput != "" {
				fmt.Fprintf(os.Stderr, "    Image:   %s\n", *v.ImageInput)
			}
			if v.RemixedFrom != nil && *v.RemixedFrom != "" {
				fmt.Fprintf(os.Stderr, "    Remix:   %s\n", *v.RemixedFrom)
			}
			fmt.Fprintln(os.Stderr)
		}
		os.Exit(0)
	}

	// Load .env automatically (if present) before reading env vars
	_ = godotenv.Load() // Ignore error if .env doesn't exist

	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "ERROR: OPENAI_API_KEY is not set")
		os.Exit(1)
	}

	if prompt == "" {
		var err error
		prompt, err = promptInteractive()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read prompt: %v\n", err)
			os.Exit(1)
		}
		if strings.TrimSpace(prompt) == "" {
			fmt.Fprintln(os.Stderr, "Prompt cannot be empty")
			os.Exit(1)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	ctx, cancel = context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	client := &http.Client{Timeout: 60 * time.Second}

	var jobID string
	var err error

	// Branch between remix and create
	if remixFrom != "" {
		// Remix existing video
		resolvedID, resolveErr := resolveRemixVideoID(remixFrom)
		if resolveErr != nil {
			fmt.Fprintf(os.Stderr, "failed to resolve remix reference: %v\n", resolveErr)
			os.Exit(1)
		}
		infof("Remixing from video: %s\n", resolvedID)
		jobID, err = remixVideo(ctx, client, baseURL, apiKey, resolvedID, prompt)
	} else {
		// Create new video
		jobID, err = createVideoJob(ctx, client, baseURL, apiKey, model, prompt, inputFile, videoSize, seconds)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "create job error: %v\n", err)
		os.Exit(1)
	}
	infof("Created job: %s\n", jobID)

	// Track start time for generation stats
	startTime := time.Now()

	// Poll for completion
	bar := progressbar.NewOptions(100,
		progressbar.OptionSetDescription("Generating video"),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionShowBytes(false),
		progressbar.OptionSetWidth(40),
		progressbar.OptionThrottle(100*time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionOnCompletion(func() {
			fmt.Fprint(os.Stderr, "\n")
		}),
		progressbar.OptionSpinnerType(14),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetRenderBlankState(true),
	)

	var downloadURL string
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "Context canceled or timed out before completion")
			os.Exit(1)
		case <-time.After(3 * time.Second):
		}

		st, err := fetchVideoStatus(ctx, client, baseURL, apiKey, jobID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "poll error: %v\n", err)
			continue
		}

		if st.Error != nil && st.Error.Message != "" {
			fmt.Fprintf(os.Stderr, "job error: %s\n", st.Error.Message)
			os.Exit(1)
		}

		// Update progress bar
		if st.Progress > 0 {
			bar.Set(st.Progress)
		}

		switch strings.ToLower(st.Status) {
		case "succeeded", "completed", "complete", "done", "ready":
			bar.Set(100)
			bar.Finish()
			// Construct the content download URL
			downloadURL = strings.TrimRight(baseURL, "/") + "/videos/" + jobID + "/content"
			goto DOWNLOAD
		case "failed", "error":
			fmt.Fprintln(os.Stderr, "Job failed")
			os.Exit(1)
		default:
			// keep polling
		}
	}

DOWNLOAD:
	if output == "" {
		// Default: save to video_id.mp4
		output = jobID + ".mp4"
	}

	if err := downloadFile(ctx, client, apiKey, downloadURL, output); err != nil {
		fmt.Fprintf(os.Stderr, "download error: %v\n", err)
		os.Exit(1)
	}

	// Report generation stats
	if output != "-" {
		duration := time.Since(startTime)
		infof("Total generation time: %s\n", formatDuration(duration))
	}

	// Save to history
	var remixFromVideoID *string
	if remixFrom != "" {
		remixFromVideoID = &remixFrom
	}
	entry := videoHistoryEntry{
		ID:          jobID,
		Prompt:      prompt,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		OutputFile:  output,
		Model:       model,
		ImageInput:  &inputFile,
		RemixedFrom: remixFromVideoID,
	}
	if inputFile == "" {
		entry.ImageInput = nil
	}
	if err := addToHistory(entry); err != nil {
		// Non-fatal: just warn
		infof("Warning: failed to save to history: %v\n", err)
	}
}

func promptInteractive() (string, error) {
	fmt.Print("Enter your video prompt: ")
	rd := bufio.NewReader(os.Stdin)
	s, err := rd.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

func createVideoJob(ctx context.Context, c *http.Client, baseURL, apiKey, model, prompt, inputFile, size, seconds string) (string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add text fields
	_ = writer.WriteField("model", model)
	_ = writer.WriteField("prompt", prompt)
	if size != "" {
		_ = writer.WriteField("size", size)
	}
	if seconds != "" {
		_ = writer.WriteField("seconds", seconds)
	}

	// Add file if provided (with dimension validation/resizing)
	if inputFile != "" {
		// Parse target dimensions from size parameter
		targetWidth, targetHeight := parseDimensions(size)

		// Process the input file based on type
		processedData, filename, mimeType, err := processInputFile(inputFile, targetWidth, targetHeight)
		if err != nil {
			return "", fmt.Errorf("processing input file: %w", err)
		}

		// Create form part with proper Content-Type header
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="input_reference"; filename="%s"`, filename)}
		h["Content-Type"] = []string{mimeType}

		part, err := writer.CreatePart(h)
		if err != nil {
			return "", fmt.Errorf("creating form part: %w", err)
		}
		if _, err := io.Copy(part, bytes.NewReader(processedData)); err != nil {
			return "", fmt.Errorf("copying file data: %w", err)
		}
	}

	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/videos", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return "", fmt.Errorf("API %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out createVideoResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", errors.New(out.Error.Message)
	}
	if out.ID == "" {
		return "", errors.New("missing job id in response")
	}
	return out.ID, nil
}

func remixVideo(ctx context.Context, c *http.Client, baseURL, apiKey, videoID, prompt string) (string, error) {
	body := remixVideoRequest{Prompt: prompt}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(baseURL, "/") + "/videos/" + videoID + "/remix"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(buf)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return "", fmt.Errorf("API %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out createVideoResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", errors.New(out.Error.Message)
	}
	if out.ID == "" {
		return "", errors.New("missing job id in response")
	}
	return out.ID, nil
}

func fetchVideoStatus(ctx context.Context, c *http.Client, baseURL, apiKey, id string) (*videoStatusResponse, error) {
	url := strings.TrimRight(baseURL, "/") + "/videos/" + id
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return nil, fmt.Errorf("API %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out videoStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func downloadFile(ctx context.Context, c *http.Client, apiKey, downloadURL, outPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return err
	}
	// Always include Authorization header for /videos/{id}/content endpoint
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return fmt.Errorf("download %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var total int64 = resp.ContentLength
	var written int64
	pr := &progressWriter{total: total, written: &written}

	if outPath == "-" {
		// Stream to stdout; only progress to stderr
		_, err = io.Copy(io.MultiWriter(os.Stdout, pr), resp.Body)
		if err != nil {
			return err
		}
		infof("\rDownloaded %s\n", humanBytes(written))
		return nil
	}

	// Ensure directory exists
	if dir := filepath.Dir(outPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	// Create temp file then rename for atomicity
	tmp := outPath + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		// best-effort cleanup on error
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()

	_, err = io.Copy(io.MultiWriter(f, pr), resp.Body)
	if err != nil {
		return err
	}
	infof("\rDownloaded %s\n", humanBytes(written))
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, outPath)
}

type progressWriter struct {
	total   int64
	written *int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n := len(b)
	nw := atomic.AddInt64(p.written, int64(n))
	if p.total > 0 {
		pct := float64(nw) / float64(p.total) * 100
		infof("\rDownloading: %s / %s (%.1f%%)", humanBytes(nw), humanBytes(p.total), pct)
	} else {
		infof("\rDownloading: %s", humanBytes(nw))
	}
	return n, nil
}

func humanBytes(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	size := float64(n)
	for _, unit := range units {
		if size < 1024 {
			return fmt.Sprintf("%.1f %s", size, unit)
		}
		size /= 1024
	}
	return fmt.Sprintf("%.1f TiB", size)
}

func detectMIMEType(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	mimeTypes := map[string]string{
		// Images (only formats supported by API)
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".png":  "image/png",
		".webp": "image/webp",
		// Videos
		".mp4":  "video/mp4",
		".webm": "video/webm",
		".mov":  "video/quicktime",
		".avi":  "video/x-msvideo",
	}
	if mime, ok := mimeTypes[ext]; ok {
		return mime
	}
	return "application/octet-stream"
}

func isImageFile(filePath string) bool {
	mime := detectMIMEType(filePath)
	return strings.HasPrefix(mime, "image/")
}

// decodeImage decodes an image from a file, using the appropriate decoder based on format
func decodeImage(filePath string) (image.Image, error) {
	ext := strings.ToLower(filepath.Ext(filePath))

	// Use chai2010/webp for WebP files (better format support than stdlib)
	if ext == ".webp" {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("reading WebP: %w", err)
		}
		return webp.Decode(bytes.NewReader(data))
	}

	// Use imaging library for other formats (JPEG, PNG, etc.)
	return imaging.Open(filePath)
}

// encodeImage encodes an image to bytes in the specified format
func encodeImage(img image.Image, ext string) ([]byte, string, string, error) {
	var buf bytes.Buffer
	var mimeType string
	var newExt string = ext

	switch ext {
	case ".webp":
		if err := webp.Encode(&buf, img, &webp.Options{Lossless: false, Quality: 90}); err != nil {
			return nil, "", "", err
		}
		mimeType = "image/webp"
	case ".png":
		if err := imaging.Encode(&buf, img, imaging.PNG); err != nil {
			return nil, "", "", err
		}
		mimeType = "image/png"
	case ".jpg", ".jpeg":
		if err := imaging.Encode(&buf, img, imaging.JPEG); err != nil {
			return nil, "", "", err
		}
		mimeType = "image/jpeg"
		newExt = ".jpg"
	default:
		// Convert unsupported formats to JPEG
		if err := imaging.Encode(&buf, img, imaging.JPEG); err != nil {
			return nil, "", "", err
		}
		mimeType = "image/jpeg"
		newExt = ".jpg"
	}

	return buf.Bytes(), newExt, mimeType, nil
}

func parseDimensions(size string) (width, height int) {
	// Default to landscape if not specified
	if size == "" {
		return 1280, 720
	}
	// Parse WxH format
	parts := strings.Split(size, "x")
	if len(parts) == 2 {
		fmt.Sscanf(parts[0], "%d", &width)
		fmt.Sscanf(parts[1], "%d", &height)
		return width, height
	}
	return 1280, 720
}

func processInputFile(filePath string, targetWidth, targetHeight int) (data []byte, filename, mimeType string, err error) {
	// Check if file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return nil, "", "", fmt.Errorf("file does not exist: %s", filePath)
	} else if err != nil {
		return nil, "", "", fmt.Errorf("checking file: %w", err)
	}

	mimeType = detectMIMEType(filePath)
	filename = filepath.Base(filePath)

	// For images: resize to exact dimensions (maintaining aspect ratio, cropping if needed)
	if isImageFile(filePath) {
		img, err := decodeImage(filePath)
		if err != nil {
			return nil, "", "", fmt.Errorf("decoding image: %w", err)
		}

		// Resize if needed (Fill maintains aspect ratio by scaling + cropping from center)
		bounds := img.Bounds()
		if bounds.Dx() != targetWidth || bounds.Dy() != targetHeight {
			img = imaging.Fill(img, targetWidth, targetHeight, imaging.Center, imaging.Lanczos)
		}

		// Encode image
		ext := strings.ToLower(filepath.Ext(filePath))
		data, newExt, mimeType, err := encodeImage(img, ext)
		if err != nil {
			return nil, "", "", fmt.Errorf("encoding image: %w", err)
		}

		// Update filename if format changed
		if newExt != ext {
			filename = strings.TrimSuffix(filename, ext) + newExt
		}

		return data, filename, mimeType, nil
	}

	// For videos: read dimensions directly from MP4 file (no external tools needed)
	currentWidth, currentHeight, err := getVideoDimensions(filePath)
	if err != nil {
		return nil, "", "", fmt.Errorf("getting video dimensions: %w", err)
	}

	// Check if resize is needed
	if currentWidth == targetWidth && currentHeight == targetHeight {
		// No resize needed - just read the file
		data, err = os.ReadFile(filePath)
		if err != nil {
			return nil, "", "", fmt.Errorf("reading video: %w", err)
		}
		return data, filename, mimeType, nil
	}

	// Need to resize - check if ffmpeg is available
	if !isFFmpegAvailable() {
		return nil, "", "", fmt.Errorf("video is %dx%d but needs to be %dx%d.\n%s",
			currentWidth, currentHeight, targetWidth, targetHeight, ffmpegInstallMsg)
	}

	// Resize video using ffmpeg
	infof("Resizing video from %dx%d to %dx%d using ffmpeg...\n", currentWidth, currentHeight, targetWidth, targetHeight)
	resizedPath, err := resizeVideoWithFFmpeg(filePath, targetWidth, targetHeight)
	if err != nil {
		return nil, "", "", fmt.Errorf("resizing video with ffmpeg: %w", err)
	}
	defer os.Remove(resizedPath) // Clean up temp file

	data, err = os.ReadFile(resizedPath)
	if err != nil {
		return nil, "", "", fmt.Errorf("reading resized video: %w", err)
	}

	return data, filename, mimeType, nil
}

func isFFmpegAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

func isFFprobeAvailable() bool {
	_, err := exec.LookPath("ffprobe")
	return err == nil
}

// getVideoDimensions returns the width and height of a video file by parsing the MP4 file directly
func getVideoDimensions(videoPath string) (width, height int, err error) {
	f, err := os.Open(videoPath)
	if err != nil {
		return 0, 0, fmt.Errorf("opening video file: %w", err)
	}
	defer f.Close()

	// Extract all tkhd boxes to find video track dimensions
	boxes, err := mp4.ExtractBoxWithPayload(f, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeTkhd()})
	if err != nil {
		return 0, 0, fmt.Errorf("extracting track header: %w", err)
	}

	// Find first valid tkhd box with dimensions
	for _, box := range boxes {
		tkhd, ok := box.Payload.(*mp4.Tkhd)
		if ok && tkhd.Width > 0 && tkhd.Height > 0 {
			// Width and Height in tkhd are stored as fixed-point 16.16 format
			width = int(tkhd.Width >> 16)
			height = int(tkhd.Height >> 16)
			return width, height, nil
		}
	}

	return 0, 0, fmt.Errorf("video dimensions not found in MP4 file")
}

func resizeVideoWithFFmpeg(inputPath string, width, height int) (string, error) {
	// Create temp file for output
	tmpFile, err := os.CreateTemp("", "sora-resized-*.mp4")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	outputPath := tmpFile.Name()
	tmpFile.Close()

	// Run ffmpeg to resize
	// -i: input file
	// -vf scale: resize filter
	// -c:v libx264: use H.264 codec
	// -crf 23: quality (lower = better, 23 is good default)
	// -preset fast: encoding speed
	// -y: overwrite output file
	cmd := exec.Command("ffmpeg",
		"-i", inputPath,
		"-vf", fmt.Sprintf("scale=%d:%d", width, height),
		"-c:v", "libx264",
		"-crf", "23",
		"-preset", "fast",
		"-an", // remove audio (Sora doesn't support it anyway)
		"-y",
		outputPath,
	)

	// Capture output for debugging
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg failed: %w\nOutput: %s", err, stderr.String())
	}

	infof("Video resized successfully\n")
	return outputPath, nil
}

// infof writes informational messages to stderr to keep stdout clean for piping
func infof(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
}

// formatDuration formats a duration in a human-readable way
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// getHistoryPath returns the path to the history file
func getHistoryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("getting home directory: %w", err)
	}
	return filepath.Join(home, ".sora-cli", "history.json"), nil
}

// loadHistory loads the history from disk
func loadHistory() (*history, error) {
	path, err := getHistoryPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &history{Videos: []videoHistoryEntry{}}, nil
		}
		return nil, fmt.Errorf("reading history: %w", err)
	}

	var h history
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("parsing history: %w", err)
	}
	return &h, nil
}

// saveHistory saves the history to disk
func saveHistory(h *history) error {
	path, err := getHistoryPath()
	if err != nil {
		return err
	}

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating history directory: %w", err)
	}

	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding history: %w", err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing history: %w", err)
	}
	return nil
}

// addToHistory adds a new entry to the history
func addToHistory(entry videoHistoryEntry) error {
	h, err := loadHistory()
	if err != nil {
		return err
	}

	// Prepend new entry (most recent first)
	h.Videos = append([]videoHistoryEntry{entry}, h.Videos...)

	// Limit to 100 most recent entries
	if len(h.Videos) > 100 {
		h.Videos = h.Videos[:100]
	}

	return saveHistory(h)
}

// resolveRemixVideoID resolves a remix reference to a video ID
// Supports: @last, @0, @1, or direct video_id
func resolveRemixVideoID(ref string) (string, error) {
	h, err := loadHistory()
	if err != nil {
		return "", fmt.Errorf("loading history: %w", err)
	}

	if len(h.Videos) == 0 {
		return "", errors.New("no videos in history")
	}

	// Handle @last shortcut
	if ref == "@last" {
		return h.Videos[0].ID, nil
	}

	// Handle @N shortcuts (e.g., @0, @1, @2)
	if strings.HasPrefix(ref, "@") {
		idxStr := strings.TrimPrefix(ref, "@")
		idx := 0
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err != nil {
			return "", fmt.Errorf("invalid index: %s", ref)
		}
		if idx < 0 || idx >= len(h.Videos) {
			return "", fmt.Errorf("index out of range: %d (have %d videos)", idx, len(h.Videos))
		}
		return h.Videos[idx].ID, nil
	}

	// Assume it's a direct video ID
	return ref, nil
}
