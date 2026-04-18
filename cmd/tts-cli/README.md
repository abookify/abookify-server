# tts-cli

Standalone Kokoro TTS client for testing voice quality and generating audio
fixtures.

## Usage

```bash
# Text file → audio (default output: chapter.af_heart.mp3 + sidecar)
tts-cli --text chapter.txt --voice af_heart

# Custom output path
tts-cli --text chapter.txt --voice bm_george --output british.mp3

# Inline text (writes tts-{voice}-{epoch}.mp3)
tts-cli "It was the best of times" --voice af_nova

# List all available voices
tts-cli --voices
```

## Output naming convention

By default tts-cli writes `<source>.<voice>.mp3` alongside the input plus
a `.tts.json` sidecar describing the synthesis.

| Input                            | Output                                    |
|----------------------------------|-------------------------------------------|
| `chapter.txt`, voice `af_heart`  | `chapter.af_heart.mp3` + `chapter.af_heart.tts.json` |
| `book.txt`, voice `bm_george`    | `book.bm_george.mp3` + `book.bm_george.tts.json`     |

## Sidecar JSON format

```json
{
  "output_mp3": "chapter.af_heart.mp3",
  "source_text": "chapter.txt",
  "voice": "af_heart",
  "word_count": 1234,
  "chunk_count": 3,
  "output_bytes": 4567890,
  "elapsed_secs": 42.1,
  "kokoro_url": "http://localhost:8880",
  "synthesized_at": "2026-04-18T16:55:00Z"
}
```

This lets you audit which voice/service produced each fixture and how long
synthesis took, handy for tracking quality regressions or voice changes.

## Round-trip testing

The expected workflow for quality testing:

```bash
# 1. Generate audio from source text
tts-cli --text source.txt --voice af_heart
# → source.af_heart.mp3 + source.af_heart.tts.json

# 2. Transcribe that audio back
stt-cli --audio source.af_heart.mp3 --whisper http://localhost:5200
# → source.af_heart.stt.json

# Now compare: source.txt vs source.af_heart.stt.json word streams.
# Ideal round-trip is near-zero WER for Kokoro (historically ~0%).
```
