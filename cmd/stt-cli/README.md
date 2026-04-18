# stt-cli

Standalone Whisper STT client for testing transcription quality on a GPU box
without touching the main server.

## Usage

```bash
# Single file — writes book.stt.json next to the input
stt-cli --audio book.mp3 --whisper http://localhost:5200 --detect-chapters

# Directory (multi-file audiobook) — processed as one logical timeline
# Writes book-dir.stt.json alongside the directory
stt-cli --audio ./audiobook-dir --detect-chapters

# Custom output path
stt-cli --audio book.mp3 --output /tmp/result.json --detect-chapters

# Or pipe JSON to stdout
stt-cli --audio book.mp3 --stdout > result.json
```

## Output sidecar convention

By default stt-cli writes a `.stt.json` sidecar next to the input:

| Input                  | Output                       |
|------------------------|------------------------------|
| `book.mp3`             | `book.stt.json`              |
| `book.m4b`             | `book.stt.json`              |
| `audiobook-dir/`       | `audiobook-dir.stt.json`     |

For directory inputs, the JSON includes a `sources` array showing how the
unified timeline maps back to individual files.

## Sharing test fixtures

When sending a fixture for debugging or regression testing, zip the audio +
its `.stt.json` together. A good structure:

```
my-fixture/
├── source.mp3          (or source/ directory)
├── source.stt.json     (stt-cli output)
├── source.epub         (optional — paired ebook)
└── meta.yaml           (optional — title, narrator, notes)
```

## Chapter detection

With `--detect-chapters`, the JSON includes a `chapters` array containing
detected chapter boundaries found from narrator patterns like "Chapter N"
or "Part N" — the same algorithm the server uses. Numbers must form a
monotonic sequence to avoid false positives from dialogue.
