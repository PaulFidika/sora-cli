# Sora CLI – Quick Start

Environment
- **Important**: To use Sora API, you must verify your organization by scanning your photo ID through OpenAI's platform.
- Set your API key:

```bash
export OPENAI_API_KEY=sk-...
```

- Or create a `.env` file in this directory (optional):

```
OPENAI_API_KEY=sk-...
```

Examples
- Save to a specific file:
```
go run . -p "A child flying a kite" -o child-kite.mp4
```

- Use Pro model (higher quality, 1080p):
```
go run . --pro -p "A child flying a kite" -o child-kite-hd.mp4
```

- Generate portrait video:
```
go run . --portrait -p "A person walking down a street" -o portrait.mp4
```

- Specify duration (4, 8, or 12 seconds):
```
go run . --seconds 8 -p "A scenic mountain landscape" -o short-video.mp4
```

- Combine options (portrait + duration + pro):
```
go run . --pro --portrait --seconds 12 -p "City lights at night" -o city-vertical.mp4
```

- Animate an image (image-to-video):
```
go run . -i photo.jpg -p "The camera pans slowly across the scene" -o animated.mp4
```

- Remix the most recent video:
```
go run . --remix @last -p "Make it sunset" -o child-kite-sunset.mp4
```

- Remix by index (0 = most recent):
```
go run . --remix @1 -p "Add dramatic clouds"
```

- Remix by filename:
```
go run . --remix child-kite.mp4 -p "Speed up the motion"
```

- List your generation history:
```
go run . --list
```

- Pipe to another command (stdout by default when no -o/-O):
```
go run . --prompt "A child flying a kite" | ffplay -i pipe:0
```

- Save and pipe simultaneously (tee):
```
go run . -p "A child flying a kite" -o - | tee child-kite.mp4 | ffmpeg -i pipe:0 -c:v libx264 -crf 18 processed.mp4
```

- Use the remote name (like curl -O):
```
go run . -p "A child flying a kite" -O
```

Notes
- Progress and status are written to stderr; stdout carries only the video bytes for piping.
- Cameos (personal likeness features) are not supported via the API - they require the Sora mobile app.
- Video generation history is stored in `~/.sora-cli/history.json` (limited to 100 most recent entries).
- Remix shortcuts: `@last` (most recent), `@0`, `@1`, etc. (by index), or filename lookup.
- Default video format is landscape (1280x720). Use `--portrait` for 720x1280 or `--landscape` to be explicit.
- Default duration is 12 seconds. Valid values: `--seconds 4`, `--seconds 8`, or `--seconds 12`.
