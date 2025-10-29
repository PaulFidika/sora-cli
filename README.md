# Sora CLI

A command-line tool for generating videos with OpenAI's Sora API.

## Installation

Install using standard Go tools:

```bash
go install github.com/fidika/sora-cli@latest
```

This installs `sora-cli` as a system binary in your `$GOPATH/bin` directory (typically `~/go/bin`).

## Configuration

**Note:**: To use Sora API, you must verify your organization by scanning your photo ID through OpenAI's platform.

Set your API key using either method:

**Option 1: Environment variable**
```bash
export OPENAI_API_KEY=sk-...
```

**Option 2: .env file (in your working directory)**
```
OPENAI_API_KEY=sk-...
```

## Usage

### 1. Generate a video from a prompt

```bash
sora-cli -p "A warrior with glowing blue energy charging up for an attack"
```

By default, this saves to `video_{id}.mp4` in your current directory.

### 2. Specify an output file

```bash
sora-cli -p "Two swordsmen clashing in a lightning storm" -o epic-battle.mp4
```

Use `-o -` to pipe to stdout; useful for composition. Note that the video will still be saved to your hard-drive in the current working dir with the default filename:
```bash
sora-cli -p "A ninja vanishing in a burst of smoke" -o - | ffplay -i pipe:0
```

### 3. Use Sora-2 Pro (better quality)

```bash
sora-cli --pro -p "A dramatic beam clash between rivals, energy crackling" -o beam-clash.mp4
```

**Note**: Pro mode is **3x more expensive** ($0.30/sec vs $0.10/sec) but delivers noticeably better quality at the same 720p resolution - sharper textures, smoother motion, richer colors, and better scene continuity.

### 4. Specify orientation and duration

**Portrait mode** (720x1280):
```bash
sora-cli --portrait -p "A martial artist performing a rising uppercut" -o uppercut.mp4
```

**Specify duration** (4, 8, or 12 seconds):
```bash
sora-cli --seconds 12 -p "A mage summoning a fireball that grows larger" -o fireball.mp4
```

**Combine options**:
```bash
sora-cli --pro --portrait --seconds 12 -p "A hero's transformation sequence with glowing aura" -o transformation.mp4
```

Default is landscape (1280x720) and 8 seconds.

### 5. Animate an image (image-to-video)

```bash
sora-cli --file tanjiro.jpg -p "The swordsman's eyes glow as energy swirls around them" -o power-up.mp4
```

**Supported formats**: JPEG, PNG, WebP
Images are automatically resized to match video dimensions (crops from center if needed). Sora-API is very specific about the dimensions of input images.

### 6. Remix a previous video

Remix a video you've already generated with Sora:

```bash
# Remix the most recent video
sora-cli --remix @last -p "Add lightning effects to the attack" -o lightning-version.mp4

# Remix by index (0 = most recent, 1 = second most recent, etc.)
sora-cli --remix @1 -p "Make the background more dramatic with storm clouds"

# Remix by video ID
sora-cli --remix video_6901abc123def456 -p "Slow down the motion for dramatic effect"

# List your generation history
sora-cli --list
```

**Important notes:**
- `--remix` only works with Sora-generated videos from your history (use `@last`, `@0`, `@1`, etc., or a video ID)
- When remixing, the **duration, resolution, and model are inherited** from the original video; you cannot ask for a longer video, for example.
- This is currently the **only way to modify videos** - video-to-video via `--file` is not yet available.

### 7. Transform an arbitrary video (video-to-video)

**⚠️ IMPORTANT: Video-to-video is currently NOT available through the Sora API.**

Using `--file` with video files will result in an error: **"Video inpaint is not available for your organization."**

This is a restricted feature that OpenAI has not yet made publicly available. According to the [official documentation](https://cookbook.openai.com/examples/sora/sora2_prompting_guide), only **image-to-video** (JPEG, PNG, WebP) is currently supported.

**To modify existing videos, use `--remix` instead** (see section 6), which works with Sora-generated videos from your history.

~~Example (not currently available):~~
```bash
# This will NOT work - video inpainting is restricted
sora-cli --file fight-scene.mp4 -p "Add energy aura effects and speed lines" -o enhanced-fight.mp4
```

## Important Notes

- **⚠️ Videos expire after 1 hour!** Once a video completes, you have ~1 hour to download it before it becomes unavailable for download. This CLI automatically downloads upon completion. Videos will still be available for remixes, however.
- **Cameos** (personal likeness features) are not supported via the API - they require the Sora mobile app.
- Video generation history is stored in `~/.sora-cli/history.json` (limited to 100 most recent entries).
