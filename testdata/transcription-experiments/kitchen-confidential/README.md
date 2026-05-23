# Kitchen Confidential — EPUB-informed transcription experiment

See `engineering/server/docs/epub-informed-transcription.md` for the writeup. This dir holds the experimental artefacts:

```
runs/
  baseline_01_0-600.json   Whisper output for slice with no prompt
  baseline.txt             extracted text
  approach1_01_0-600.json  Whisper output for slice with EPUB-derived initial_prompt
  approach1.txt            extracted text
  slice_01_0-600.mp3       (gitignored) 10-min slice of 01.mp3
epub-text/                 (gitignored) plain-text extraction of EPUB parts
prompt_v1.txt              the initial_prompt that produced approach1
```

## Reproducing

Audio source: `/mnt/raid/audiobooks/Anthony Bourdain - Kitchen Confidential Audiobook/01.mp3`.

```bash
# 1. Cut the test slice
ffmpeg -y -ss 0 -t 600 -i ".../01.mp3" -c copy runs/slice_01_0-600.mp3

# 2. Run baseline
curl -X POST http://localhost:5200/transcribe \
  -F "file=@runs/slice_01_0-600.mp3" \
  -F "word_timestamps=true" -F "language=en" \
  -o runs/baseline_01_0-600.json

# 3. Run approach 1
curl -X POST http://localhost:5200/transcribe \
  -F "file=@runs/slice_01_0-600.mp3" \
  -F "word_timestamps=true" -F "language=en" \
  --form-string "initial_prompt=$(cat prompt_v1.txt)" \
  -o runs/approach1_01_0-600.json
```

Service needs the `initial_prompt` form-field support added in `services/whisper/server.py` (commit `bbf7d77`).
