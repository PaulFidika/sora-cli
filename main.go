package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/joho/godotenv"
	"github.com/schollz/progressbar/v3"
)

const (
	defaultBaseURL = "https://api.openai.com/v1"
)

type createVideoRequest struct {
	Model            string  `json:"model"`
	Prompt           string  `json:"prompt"`
	Image            *string `json:"image,omitempty"`               // base64-encoded image data URI
	RemixFromVideoID *string `json:"remix_from_video_id,omitempty"` // video ID to remix from
	Size             *string `json:"size,omitempty"`                // e.g. "1280x720", "720x1280"
	Seconds          *string `json:"seconds,omitempty"`             // duration in seconds, e.g. "8"
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
		prompt       string
		output       string
		useRemote    bool
		model        string
		usePro       bool
		baseURL      string
		timeout      time.Duration
		pollInterval time.Duration
		imageFile    string
		remixFrom    string
		listHistory  bool
		seconds      string
		portrait     bool
		landscape    bool
	)

	flag.StringVar(&prompt, "prompt", "", "Text prompt for the video. If empty, reads interactively.")
	flag.StringVar(&prompt, "p", "", "Shorthand for --prompt")
	flag.StringVar(&output, "output", "", "Write output to <file>. Use '-' for stdout. Default is stdout unless -O is used.")
	flag.StringVar(&output, "o", "", "Shorthand for --output")
	flag.BoolVar(&useRemote, "O", false, "Write output to a file named by the remote (derived from URL/job id)")
	flag.StringVar(&imageFile, "image", "", "Path to input image file for image-to-video generation")
	flag.StringVar(&imageFile, "i", "", "Shorthand for --image")
	flag.StringVar(&remixFrom, "remix", "", "Remix from video (@last, @1, filename, or video_id)")
	flag.BoolVar(&listHistory, "list", false, "List generation history and exit")
	flag.StringVar(&model, "model", "sora-2", "Video model")
	flag.BoolVar(&usePro, "pro", false, "Use sora-2-pro model (higher quality, 1080p)")
	flag.StringVar(&seconds, "seconds", "12", "Video duration in seconds: 4, 8, or 12 (default: 12)")
	flag.BoolVar(&portrait, "portrait", false, "Generate portrait video (720x1280)")
	flag.BoolVar(&landscape, "landscape", false, "Generate landscape video (1280x720, default)")
	flag.StringVar(&baseURL, "base-url", defaultBaseURL, "OpenAI API base URL")
	flag.DurationVar(&timeout, "timeout", 15*time.Minute, "Overall timeout for job")
	flag.DurationVar(&pollInterval, "poll-interval", 3*time.Second, "Polling interval for job status")
	flag.Parse()

	// Validate seconds
	if seconds != "4" && seconds != "8" && seconds != "12" {
		fmt.Fprintf(os.Stderr, "Invalid --seconds value: %s (must be 4, 8, or 12)\n", seconds)
		os.Exit(2)
	}

	// Override model if --pro is set
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
		// Default to landscape
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

	// Disallow combining -o/--output with -O, like curl
	if output != "" && useRemote {
		fmt.Fprintln(os.Stderr, "Cannot use --output/-o with -O")
		os.Exit(2)
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

	ctx, cancel = context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{Timeout: 60 * time.Second}

	// Process image file if provided
	var imageDataURI *string
	if imageFile != "" {
		dataURI, err := encodeImageToDataURI(imageFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to process image: %v\n", err)
			os.Exit(1)
		}
		imageDataURI = &dataURI
	}

	// Resolve remix reference if provided
	var remixVideoID *string
	if remixFrom != "" {
		resolvedID, err := resolveRemixVideoID(remixFrom)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to resolve remix reference: %v\n", err)
			os.Exit(1)
		}
		remixVideoID = &resolvedID
		infof("Remixing from video: %s\n", resolvedID)
	}

	// Prepare optional parameters
	var sizeParam, secondsParam *string
	if videoSize != "" {
		sizeParam = &videoSize
	}
	if seconds != "" {
		secondsParam = &seconds
	}

	jobID, err := createVideoJob(ctx, client, baseURL, apiKey, model, prompt, imageDataURI, remixVideoID, sizeParam, secondsParam)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create job error: %v\n", err)
		os.Exit(1)
	}
	infof("Created job: %s\n", jobID)

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
		case <-time.After(pollInterval):
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
		if useRemote {
			// Use video ID as filename
			output = jobID + ".mp4"
		} else {
			// curl-like default: write to stdout when no -o/-O
			output = "-"
		}
	}

	if err := downloadFile(ctx, client, apiKey, downloadURL, output); err != nil {
		fmt.Fprintf(os.Stderr, "download error: %v\n", err)
		os.Exit(1)
	}
	if output != "-" {
		infof("Saved: %s\n", output)
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
		ImageInput:  &imageFile,
		RemixedFrom: remixFromVideoID,
	}
	if imageFile == "" {
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

func createVideoJob(ctx context.Context, c *http.Client, baseURL, apiKey, model, prompt string, image *string, remixFromVideoID *string, size *string, seconds *string) (string, error) {
	body := createVideoRequest{Model: model, Prompt: prompt, Image: image, RemixFromVideoID: remixFromVideoID, Size: size, Seconds: seconds}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/videos", strings.NewReader(string(buf)))
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

func encodeImageToDataURI(filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	ext := strings.ToLower(filepath.Ext(filePath))
	mimeType := map[string]string{".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif", ".webp": "image/webp"}[ext]
	if mimeType == "" {
		return "", fmt.Errorf("unsupported format: %s", ext)
	}
	return fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(data)), nil
}

// infof writes informational messages to stderr to keep stdout clean for piping
func infof(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
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
// Supports: @last, @0, @1, filename.mp4, or direct video_id
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

	// Try to match by output filename
	for _, v := range h.Videos {
		if v.OutputFile == ref || filepath.Base(v.OutputFile) == ref {
			return v.ID, nil
		}
	}

	// Assume it's a direct video ID
	return ref, nil
}
