# abookify Server

The core of abookify. Runs on the user's desktop, NAS, or home server as a single Go binary with embedded assets.

**License:** [AGPL-3.0](LICENSE). Free for personal and commercial use; modifications and network-deployed forks must publish source. Mobile and desktop client apps are distributed separately under their own licenses.

> **Setting it up?** See **[SETUP.md](SETUP.md)** — a getting-started guide for
> self-hosters: install via Docker Compose, add your library, local engines vs.
> API keys, remote access, and optional auth.

## Components

### Core Runtime
- Single-binary distribution with embedded assets (Go `embed` package)
- Cross-compilation targets: Linux x86_64, Linux ARM64, macOS Intel, macOS Apple Silicon, Windows x86_64
- Service/daemon mode (systemd on Linux, launchd on macOS, Windows Service)
- Desktop tray app wrapper for non-technical users (e.g., Wails, Fyne, or systray)
- Auto-update mechanism with opt-out

### Library Management
- File ingestion and monitoring (watched folders via fsnotify)
- Format detection and validation: EPUB, PDF, MP3, M4B, FLAC, AAC
- DRM detection and graceful rejection
- Metadata extraction (ID3 tags, EPUB metadata, embedded chapters)
- Cover art extraction and thumbnail generation
- Duplicate detection
- Series and author grouping
- Tags and custom collections

### Storage Layer
- File storage on local disk (configurable paths)
- **SQLite** database for metadata, positions, annotations
- **sqlite-vec** for vector embeddings (RAG)
- Cache management for generated TTS audio and Whisper transcripts
- Backup and restore functionality
- Optional encryption at rest (user-controlled key)

### Text Processing
- EPUB parser and text extraction
- PDF text extraction (including OCR fallback via Tesseract for scanned PDFs)
- Chapter detection and boundary mapping
- Text chunking for embeddings (semantic or fixed-size with overlap)
- Language detection
- Embedding generation (local model via Ollama/llama.cpp, or cloud API with BYOK)

### Audio Processing
- Whisper STT for audiobook transcription (whisper.cpp for CPU, faster-whisper for GPU)
- Word-level timestamp generation
- Forced alignment (aeneas) for audiobook-to-ebook sync maps
- Audio file transcoding (FFmpeg) for mobile streaming optimization
- Chapter marker extraction from M4B
- Audio waveform generation for scrubbing UI

### TTS Engine
- Kokoro integration for high-quality local TTS (GPU preferred, CPU fallback)
- Piper integration for lightweight CPU TTS
- Hardware detection to auto-select best engine
- Voice selection and preview
- SSML handling for pauses, emphasis
- Chunked generation with on-the-fly streaming
- Generated audio caching to disk
- BYOK cloud TTS fallback (ElevenLabs, OpenAI, Google)

### AI / RAG Layer
- Retrieval pipeline: position-aware + semantic similarity
- Context assembly for LLM queries (recent playback + retrieved chunks)
- LLM integration (BYOK for OpenAI, Anthropic, Gemini; optional local via Ollama)
- Function-calling endpoints for Gemini Live to query book content
- Conversation history persistence
- Citation generation with timestamps/page numbers

### API Server
- REST API for mobile clients
- WebSocket for real-time sync events
- Authentication (user accounts, device pairing tokens)
- Rate limiting
- CORS and security headers
- OpenAPI spec for documentation

### Web UI
- Library browser
- Setup wizard (first-run experience)
- Settings panel
- Device pairing (QR code generation)
- Manual metadata editing
- Processing queue status (transcription, TTS generation progress)
- BYOK key management
- Usage statistics

### Sync Engine
- Playback position sync with conflict resolution
- Annotations sync (CRDT-based or append-log)
- Bookmark sync
- Cross-format position mapping (audiobook <-> ebook via alignment)
- Offline queue for pending syncs from mobile
