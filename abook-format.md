# .abook Format Specification (Draft)

> **The canonical spec now lives at https://github.com/abookify/abook-format.**
> This file is a vendored copy kept alongside the reference implementation. For the latest version, contributing, JSON Schemas, and version tags, see the spec repo.

A lightweight, open container format that bundles an audiobook with its ebook counterpart and word-level synchronization data. Designed to be simple, portable, and tooling-friendly.

> **Try one:** download a free public-domain sample `.abook` from the [showcase](https://abookify.com/showcase) (or the [`showcase-v1` release](https://github.com/abookify/abookify-server/releases/tag/showcase-v1)), then open it in your browser at [abookify.com/open](https://abookify.com/open) or inspect it with `abook info <file>`.

## Container

An `.abook` file is a **ZIP archive** with a `.abook` extension. Standard ZIP tools can open it.

## Structure

```
book.abook
├── manifest.json
├── text/
│   ├── chapter-001.html
│   ├── chapter-002.html
│   └── ...
├── audio/
│   ├── chapter-001.mp3
│   ├── chapter-002.mp3
│   └── ...
└── sync/
    ├── chapter-001.json
    ├── chapter-002.json
    └── ...
```

## manifest.json

```json
{
  "format": "abook",
  "version": 1,
  "title": "Frankenstein; or, The Modern Prometheus",
  "author": "Mary Wollstonecraft Shelley",
  "language": "en",
  "description": "The 1818 edition.",
  "created": "2026-04-12T00:00:00Z",
  "generator": "abookify v0.1.0",
  "chapters": [
    {
      "index": 0,
      "title": "Letter 1",
      "text": "text/chapter-001.html",
      "audio": "audio/chapter-001.mp3",
      "sync": "sync/chapter-001.json",
      "duration_secs": 245.3,
      "word_count": 1204
    }
  ],
  "tts_voice": "en_US-lessac-high",
  "stt_model": "whisper-large-v3",
  "source": {
    "text_origin": "Project Gutenberg #84",
    "audio_origin": "LibriVox (read by Cori Samuel)"
  }
}
```

## Text Files (text/*.html)

Clean, semantic HTML. No external dependencies. Minimal markup:

```html
<h1>Letter 1</h1>
<p>To Mrs. Saville, England.</p>
<p>St. Petersburgh, Dec. 11th, 17—.</p>
<p>You will rejoice to hear that no disaster has accompanied
the commencement of an enterprise which you have regarded
with such evil forebodings...</p>
```

Each word that has sync data gets a `data-w` attribute with its word index:

```html
<p><span data-w="0">You</span> <span data-w="1">will</span> <span data-w="2">rejoice</span>...</p>
```

The `data-w` attribute is optional — readers that don't support sync just render the HTML normally.

## Audio Files (audio/*.mp3)

Standard MP3 (or M4A/FLAC/WAV — format indicated by extension). One file per chapter. Recommended: 64-128 kbps MP3 for spoken word.

## Sync Files (sync/*.json)

Word-level timestamp mapping. Compact array format to keep file size down:

```json
{
  "format": "word_timestamps",
  "version": 1,
  "words": [
    [0.00, 0.25, "You"],
    [0.25, 0.52, "will"],
    [0.52, 1.10, "rejoice"],
    [1.10, 1.28, "to"],
    [1.28, 1.65, "hear"]
  ]
}
```

Each entry: `[start_seconds, end_seconds, "word"]`

This is the most compact reasonable JSON representation:
- ~25 bytes per word average
- 75,000-word book ≈ 1.9 MB sync data
- 300,000-word book ≈ 7.5 MB sync data

### Future: Paragraph-level sync (optional, supplementary)

For readers that only need paragraph-level sync (lighter, sufficient for highlight-as-you-listen):

```json
{
  "format": "paragraph_timestamps",
  "version": 1,
  "paragraphs": [
    [0.00, 12.5, 0, 45],
    [12.5, 28.3, 46, 102]
  ]
}
```

Each entry: `[start_secs, end_secs, first_word_index, last_word_index]`

## Size Estimates

For a typical novel (~80,000 words, ~10 hours audio):

| Component | Size |
|---|---|
| Text (HTML) | ~500 KB |
| Audio (64kbps MP3) | ~280 MB |
| Sync (word-level JSON) | ~2 MB |
| manifest.json | ~5 KB |
| **Total** | **~283 MB** |

The sync data is negligible (<1%) compared to the audio.

## Design Principles

1. **ZIP-based**: Any tool can open it. No custom binary format.
2. **JSON for metadata and sync**: Human-readable, easy to generate and parse.
3. **HTML for text**: Renderable in any browser or webview. No custom markup language.
4. **Graceful degradation**: Without sync files, it's still a usable audiobook + ebook. Without audio, it's an ebook. Without text, it's an audiobook.
5. **No DRM**: This is an open format for user-owned content.
6. **Streamable chapters**: Each chapter is independent — a reader can start playing chapter 5 without loading chapters 1-4.

## MIME Type

`application/x-abook+zip` (proposed)

## Implementation Notes

- abookify generates .abook files after TTS/STT + alignment completes
- Import: unzip, read manifest, ingest into library
- Export: bundle existing work data into .abook
- The format is versioned; readers should check `version` field

## Command-line tool (`abook`)

A standalone companion CLI ships with abookify-server (`cmd/abook`) for
inspecting, extracting, and building `.abook` files without a running server.
It's a static, dependency-free binary (pure-Go SQLite, no CGO).

```
abook info <file.abook> [--json]   print manifest + source/file summary
abook extract <file.abook> [dir]   unzip the archive (default: <name>/)
abook pack <dir> [out.abook]       build a .abook from an unpacked directory
abook version
```

- **`info`** reads `manifest.json`, lists the ZIP members, and summarizes
  `book.db` (sources, chapter counts, RAG chunks + how many carry embeddings,
  alignment coverage). `--json` emits the same data as JSON. It also flags if
  any entry uses a non-DEFLATE compression method — all standard `.abook` files
  are STORE/DEFLATE, so they open in any ZIP tool or in a browser via
  `DecompressionStream`.
- **`extract`** unzips to a directory (zip-slip guarded). Because a `.abook` is
  a standard deflate ZIP, `unzip file.abook` works too.
- **`pack`** zips an unpacked directory back into a `.abook`, recomputing
  `book.db`'s `sha256` in the manifest so an edited `book.db` stays consistent.

**Build** (cross-platform static binaries into `dist/`):

```
make build-abook                                   # all platforms
make build-abook PLATFORMS="linux/amd64 darwin/arm64"
```
