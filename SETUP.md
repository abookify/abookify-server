# Abookify — Setup & Getting Started

A self-hosted audiobook + ebook library with word-level audio↔text sync, AI
Q&A, and cross-device playback. You bring your own content; everything runs on
your hardware. This guide is for a **technical self-hoster** comfortable with a
terminal and Docker. (A one-click desktop app is in the works — see
[Desktop app](#desktop-app-coming-soon).)

> **License:** the server is AGPL-3.0. Free for personal and commercial use;
> network-deployed forks must publish their source.

---

## 1. What you're running

`docker compose` brings up three services:

| Service   | Port | Purpose                                   | Idle RAM |
|-----------|------|-------------------------------------------|----------|
| `server`  | 7654 | The app: web UI, API, library, database   | ~150 MB  |
| `whisper` | 5200 | Speech‑to‑text (transcribe audiobooks)    | ~3 GB    |
| `kokoro`  | 8880 | Text‑to‑speech (generate audio from text) | ~1 GB    |

Two more are **opt-in** and off by default: `nullbore` (remote-access relay) and
`booknlp` (experimental cast-of-characters). Both are covered below.

**Requirements:** Docker + Docker Compose, ~10 GB free disk for the images and
models (more for your library and generated audio), and 4 GB+ RAM (8 GB+
recommended if you transcribe on CPU). A GPU is optional but greatly speeds up
transcription — see [GPU](#gpu-acceleration-optional).

---

## 2. Quick start

```bash
# 1. Get the code
git clone https://github.com/abookify/abookify-server.git
cd abookify-server          # this is engineering/server/ in the monorepo

# 2. Build and start everything (first build pulls/builds the ML images — slow once)
docker compose up --build -d

# 3. Watch it come up
docker compose logs -f server      # Ctrl-C to stop tailing
curl http://localhost:7654/api/health
```

Then open **http://localhost:7654** in a browser. Settings live at
**http://localhost:7654/settings.html**.

First boot builds the Whisper and Kokoro images and downloads their models on
first use, so the very first transcription/synthesis is slower than later runs.

To stop everything: `docker compose down` (your library, database, and generated
audio persist — see [Where your data lives](#where-your-data-lives)).

---

## 3. Add your content

Abookify watches a **library directory** and ingests anything you drop in —
no manual import step. By default the Compose file mounts
`./testdata/library` into the container as `/library`.

**Point it at your own library** by editing the `server` service volume in
`docker-compose.yml`:

```yaml
    volumes:
      # change the left side to wherever your books live:
      - /path/to/your/library:/library
```

Then `docker compose up -d server` to apply. Now copy audiobooks and ebooks into
that folder (subfolders are fine — one folder per book works well):

- **Audio:** `.mp3`, `.m4a` / `.m4b`, `.flac`, `.aac`, `.opus` / `.ogg`
- **Ebooks:** `.epub` (best), `.txt`; `.mobi` / `.azw3` / `.azw` are auto-converted
  to EPUB on import (via Calibre, bundled in the image)
- **PDF** is catalogued but not chapter-split or aligned — convert to EPUB first

The file watcher picks up new files within a second or two; the library view
updates live. Embedded metadata (ID3 tags, EPUB metadata, M4B chapter markers)
and cover art are extracted automatically, and audiobooks are auto-matched with
their matching ebook into a single **work**.

If a network share (NFS/SMB/sshfs) doesn't fire filesystem events reliably, use
**Settings → Rescan now** to force a sweep.

### Make the audio↔text magic happen

The headline feature — word-level karaoke and citations that point at both an
audio timestamp and a book page — needs two things per book:

1. A **transcript**: run STT on the audiobook (the work menu → transcribe, or it
   happens automatically when you add audio with a local Whisper engine running).
2. **Alignment**: once a transcript and an ebook exist for the same work,
   abookify auto-aligns them. The **coverage %** on the work card is your
   quality readout. You can re-run it from the work menu or **Settings →
   Re-align all works**.

CPU transcription runs roughly 0.8× real-time (a 10‑hour book ≈ 12 hours).
A GPU does ~8–15× real-time. See [GPU](#gpu-acceleration-optional).

---

## 4. Engines: local vs. API keys

**Speech (STT/TTS)** runs **locally** in the `whisper` and `kokoro` containers —
no account, no API key, nothing leaves your machine. This is the default and
needs no configuration. (Cloud STT/TTS providers are not wired up yet.)

**Book Q&A (the LLM / RAG features)** is **bring-your-own-key**. Without a key
you still get keyword search; with one you get conversational Q&A grounded in the
book, with citations. Configure it in **Settings → Book Q&A**:

- **OpenAI** or **Anthropic** — paste an API key.
- **Ollama** — point at a local Ollama instance for a fully offline LLM
  (set the base URL, e.g. `http://host.docker.internal:11434`).

Use **Test connection** to verify, and **Clear** to wipe a stored key. When you
enable a provider, abookify embeds your library in the background so Q&A is ready
to use; new books are embedded automatically as they're added.

### GPU acceleration (optional)

On a host with an NVIDIA GPU and the `nvidia-container-toolkit` installed, run
Whisper with CUDA via the overlay:

```bash
docker compose -f docker-compose.yml -f docker-compose.gpu.yml up -d --build whisper
```

This switches Whisper to `float16` on the GPU — ~10× faster transcription with
an identical transcript. Everything else is unchanged.

**Free up RAM when idle:** Whisper holds ~3 GB even when not transcribing. If
you're done processing for a while: `docker compose stop whisper`
(bring it back with `docker compose up -d whisper`).

---

## 5. Remote access (use it from your phone / away from home)

Abookify is tunnel-agnostic — the mobile app and web UI work against **any**
reachable URL. Pick whichever fits you:

- **Just on your LAN:** open `http://<server-lan-ip>:7654` from any device on the
  same network. Nothing else needed.
- **A VPN / mesh (recommended for privacy):** Tailscale, WireGuard, etc. — reach
  the server by its mesh IP. No ports exposed to the internet.
- **A reverse-tunnel:** Cloudflare Tunnel, or the built-in **NullBore** relay.
- **Port-forward:** forward 7654 on your router (turn on auth first — see below).

### Built-in NullBore relay

To expose the server through a NullBore relay (gives you a stable
`https://<id>.abookify.nullbore.com` URL), put your relay credentials in a
`.env` file next to `docker-compose.yml`:

```bash
# .env
NULLBORE_API_KEY=your-relay-api-key
# optional overrides:
# NULLBORE_SERVER=https://tunnel.nullbore.com
# NULLBORE_TUNNELS=server:7654
```

Then start the relay profile:

```bash
docker compose --profile relay up -d nullbore
```

The server mints a stable per-install slug automatically. You can **rotate** the
public URL anytime from **Settings → Mobile App Pairing → Rotate tunnel URL**
(this invalidates the old URL; paired devices must re-scan, and you restart the
relay container so it advertises the new slug).

### Pair the mobile app

In **Settings → Mobile App Pairing**, scan the QR code from the abookify app, or
type the server URL in by hand. The QR encodes the public URL plus a short‑lived
pairing token (and, when auth is on, a login token so the phone is signed in on
scan).

> **Anything you expose to the internet should have auth enabled.** See next.

---

## 6. Optional password protection

Auth is **off by default** (an open server, which is fine on a trusted LAN/VPN).
Before exposing the server publicly, turn it on in **Settings → Security**:

- Set a username + password. The password is hashed (bcrypt); only the hash is
  stored, and it's never sent back to a client.
- Once enabled, the whole server is gated: the web UI shows a login screen, and
  the mobile app authenticates with a token (media streaming and the live sync
  socket are covered too).
- To disable, clear the password in the same place (back to an open server).

---

## 7. Operations

### Health & status

```bash
curl http://localhost:7654/api/health   # liveness + per-engine (tts/stt) status
curl http://localhost:7654/api/ready     # 200 once booted (used by the desktop shell)
```

The web UI also has a **System Console** (Settings) showing live server logs.

### Updating

```bash
git pull
docker compose up --build -d
```

Your database, library, and generated audio are untouched by a rebuild.

### Where your data lives

| Data                     | Location                                              |
|--------------------------|-------------------------------------------------------|
| Database (everything)    | `./data/abookify.db` (SQLite, WAL mode)               |
| Your library             | the host folder you mounted at `/library`             |
| Generated TTS audio      | the `generated-audio` Docker volume                   |
| Whisper / Kokoro models  | the `whisper-models` / `kokoro-models` Docker volumes |

**Backup** = copy `./data/abookify.db` (and your library folder). It's a single
SQLite file; copying it while the server is stopped is the simplest safe backup.
You can also produce portable, per-book **`.abook`** bundles from
**Settings → Offline export** (or `POST /api/export-all`).

### Experimental: cast of characters

An opt-in BookNLP service extracts a per-book character list (names + aliases).
It's a large image (~6.5 GB), CPU-heavy, and labelled **experimental**
everywhere (alias detection over-splits on some books). To try it:

```bash
docker compose --profile booknlp up -d booknlp
```

Then enable it in **Settings → Cast** and extract per work. Stop the container
(`docker compose stop booknlp`) to reclaim the RAM when you're done;
already-extracted casts stay visible read-only.

---

## 8. Troubleshooting

- **Web UI loads but shows no books:** check the library mount path in
  `docker-compose.yml` and `docker compose logs server` for scan errors; try
  **Settings → Rescan now**.
- **Transcription never finishes / is very slow:** that's CPU at ~0.8× real‑time.
  Use the [GPU overlay](#gpu-acceleration-optional), or leave it running — it
  resumes across restarts (the job queue is persistent).
- **Engine dots red in Settings:** `docker compose ps` — make sure `whisper`/
  `kokoro` are up; they can take a minute to load models on first request.
- **Out of RAM:** stop Whisper between transcription runs (above); it's the
  heaviest component.
- **Phone can't connect:** confirm the URL is reachable from the phone's network
  (LAN IP vs relay URL), and that auth — if on — isn't blocking the pairing token.

---

## Desktop app (coming soon)

A one-click desktop app (Tauri) is in development — it bundles the server and a
self-contained ML engine (with optional GPU) and manages everything as a
background process, so non-technical users won't need Docker or a terminal.
Linux builds (AppImage/.deb) are already produced in CI; macOS and Windows
installers follow once code-signing is in place. Until then, this Compose setup
is the way to run abookify.

---

*Questions, contributions, and issues: see the project README and the
`abookify-server` repository.*
