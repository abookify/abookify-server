# Sidecar v2: Timed Event Stream

## Motivation

v1 sidecars store word timestamps as `[{s, e, w}, ...]`. Silences between
words are computed by `words[i+1].s - words[i].e`, which is unreliable
because Whisper interpolates word boundaries within segments (the gap can
show 0.000s even when the narrator paused for 2 seconds).

v2 makes silences first-class events alongside words, measured independently
by ffmpeg silencedetect (or Whisper VAD). Every downstream algorithm —
chapter detection, paragraph breaks, karaoke highlighting, title extraction
— reads the event stream directly instead of computing gaps.

## Schema

```jsonc
{
  "version": 2,
  "language": "en",
  "duration": 27586.584,        // total audio duration in seconds

  // The event stream. Ordered by time. Heterogeneous types.
  "events": [
    // Word event — one per transcribed word.
    {
      "type": "word",
      "s": 21.53,               // start time (seconds)
      "e": 21.89,               // end time (seconds)
      "w": " Chapter",          // word text (leading space = word boundary)
      "conf": 0.97              // Whisper's per-word confidence (0.0–1.0)
    },

    // Silence event — a real acoustic pause detected by VAD/silencedetect.
    {
      "type": "silence",
      "s": 22.15,               // silence start
      "e": 23.20,               // silence end
      "duration": 1.05,         // convenience (= e - s)
      "source": "vad",          // detector: "vad" | "silencedetect" | "both"
      "rms_db": -42.3,          // average loudness during silence (optional)
      "kind": ""                // classified later: "breath" | "sentence" |
                                //   "paragraph" | "chapter" | "" (unknown)
    },

    // Future: non-speech audio events.
    // { "type": "music", "s": 0.0, "e": 15.2, "label": "opening theme" },
    // { "type": "sfx",   "s": 340.1, "e": 341.5, "label": "door slam" }
  ],

  // Convenience views derived from events (redundant but useful for
  // quick access without iterating the whole stream).
  "words": [...],               // flat list of word events only (backward compat)
  "silences": [...],            // flat list of silence events only

  // Chapter detection results (narrator-pattern + silence-confirmed).
  // Same shape as v1 but with richer confidence and kind fields.
  "chapters": [
    {
      "title": "Chapter 2 Caffeine, Jet Lag and Melatonin",
      "start_sec": 1434.09,
      "end_sec": 4959.44,
      "word_idx": 3512,         // index into events[] (type=word only)
      "silence_idx": 42,        // index into events[] of the preceding silence
      "confidence": 0.92,       // combined: narrator-pattern + silence boost
      "method": "narrator+silence"  // how it was detected
    }
  ],

  // Multi-file source mapping (unchanged from v1).
  "sources": [
    {
      "file": "01.mp3",
      "offset_secs": 0.0,
      "duration_secs": 4554.264
    }
  ]
}
```

## Detection pipeline (in stt-cli)

```
Audio file(s)
     │
     ├──► Whisper (word_timestamps=True, vad_filter=True)
     │       └─► word events [{type:"word", s, e, w, conf}, ...]
     │
     ├──► ffmpeg silencedetect (noise=-30dB, d=0.15)
     │       └─► silence candidates [{s, e, duration, rms_db}, ...]
     │
     ├──► (optional) Whisper VAD segments
     │       └─► VAD silence intervals
     │
     └──► Merge step:
            1. Interleave words + silences by time.
            2. Tag silences with "source" (vad / silencedetect / both).
            3. Classify silence.kind by duration:
                 ≥ 3.0s  → "chapter"
                 ≥ 0.6s  → "paragraph"
                 ≥ 0.3s  → "sentence"
                 < 0.3s  → "breath"
            4. Run narrator-pattern chapter detection on word events.
            5. Cross-reference: chapter candidates with adjacent "chapter"
               silences get confidence boost.
            6. Emit final chapters[] array.
```

## Backward compatibility

- `"version": 2` distinguishes from v1 (which has no version field).
- `"words"` array is still present for consumers that only need word
  timestamps (karaoke, search, alignment).
- Server sidecar importer checks version:
  - No version field → v1 path (current code).
  - version=2 → v2 path (reads events[], silences[], chapters[]).

## File naming

Same convention: `<audio>.stt.json` or `<dir>.stt.json`.

## Size estimate

v1 Why We Sleep sidecar: 3.3 MB (69k words).
v2 adds ~2k silence events × ~80 bytes each = ~160 KB overhead (< 5%).
