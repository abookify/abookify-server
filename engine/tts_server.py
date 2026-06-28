"""Kokoro TTS HTTP service (hermetic-engine edition).

OpenAI-compatible surface, byte-compatible with what the Go server
(internal/tts) and tts-cli already call against kokoro-fastapi:

  GET  /v1/models        -> {"object":"list","data":[{"id":"kokoro",...}]}
  POST /v1/audio/speech  JSON: {model, input, voice, response_format} -> audio bytes

We back it with the `kokoro` PyPI package (hexgrad/Kokoro-82M) instead of the
full kokoro-fastapi app, so the bundle stays small and hermetic. GPU is
auto-detected via torch (CUDA -> cuda, else cpu). Audio is encoded with PyAV
(bundled ffmpeg libs) so no system ffmpeg is required. Model weights live under
~/.abookify/models/kokoro.
"""
import io
import os
import threading
from pathlib import Path

MODELS_DIR = Path(
    os.environ.get("ABOOKIFY_MODELS_DIR", Path.home() / ".abookify" / "models")
)
KOKORO_CACHE = MODELS_DIR / "kokoro"
KOKORO_CACHE.mkdir(parents=True, exist_ok=True)
# Route all HuggingFace downloads (kokoro weights + voices) into our bundle dir.
os.environ.setdefault("HF_HOME", str(KOKORO_CACHE))

import numpy as np  # noqa: E402
import av  # noqa: E402  (PyAV — pulled in by faster-whisper; bundles ffmpeg)
import torch  # noqa: E402
from flask import Flask, request, jsonify, Response  # noqa: E402
from kokoro import KPipeline  # noqa: E402

app = Flask(__name__)

SAMPLE_RATE = 24000  # Kokoro emits 24 kHz mono float32


def detect_device():
    dev = os.environ.get("ABOOKIFY_TTS_DEVICE")
    if dev:
        return dev
    try:
        if torch.cuda.is_available():
            return "cuda"
    except Exception as e:  # noqa: BLE001
        print(f"[tts] CUDA probe failed ({e}); using CPU")
    return "cpu"


DEVICE = detect_device()
print(f"[tts] kokoro device={DEVICE}; model cache: {KOKORO_CACHE}")

# One pipeline per lang_code (voice prefix: a=US English, b=UK English, ...).
_pipelines: dict[str, KPipeline] = {}


def pipeline_for(voice: str) -> KPipeline:
    lang = voice[0] if voice else "a"
    if lang not in _pipelines:
        print(f"[tts] init KPipeline lang_code={lang} on {DEVICE}")
        _pipelines[lang] = KPipeline(lang_code=lang, device=DEVICE)
    return _pipelines[lang]


def synth(text: str, voice: str) -> np.ndarray:
    pipe = pipeline_for(voice)
    chunks = [audio for _, _, audio in pipe(text, voice=voice)]
    if not chunks:
        return np.zeros(0, dtype=np.float32)
    arr = np.concatenate([np.asarray(c, dtype=np.float32) for c in chunks])
    return arr


def encode(audio: np.ndarray, fmt: str) -> tuple[bytes, str]:
    """Encode float32 mono @ 24kHz to the requested container.

    Supports mp3 (default, what the Go client asks for), wav, opus/flac/aac.
    Returns (bytes, mime).
    """
    fmt = (fmt or "mp3").lower()
    container_fmt = {"mp3": "mp3", "wav": "wav", "opus": "ogg", "flac": "flac", "aac": "adts"}.get(fmt, "mp3")
    codec = {"mp3": "libmp3lame", "wav": "pcm_s16le", "opus": "libopus", "flac": "flac", "aac": "aac"}.get(fmt, "libmp3lame")
    mime = {"mp3": "audio/mpeg", "wav": "audio/wav", "opus": "audio/ogg", "flac": "audio/flac", "aac": "audio/aac"}.get(fmt, "audio/mpeg")

    buf = io.BytesIO()
    out = av.open(buf, mode="w", format=container_fmt)
    stream = out.add_stream(codec, rate=SAMPLE_RATE)
    stream.layout = "mono"

    # int16 PCM frame
    pcm = np.clip(audio, -1.0, 1.0)
    pcm16 = (pcm * 32767.0).astype(np.int16).reshape(1, -1)
    frame = av.AudioFrame.from_ndarray(pcm16, format="s16", layout="mono")
    frame.rate = SAMPLE_RATE
    for packet in stream.encode(frame):
        out.mux(packet)
    for packet in stream.encode(None):  # flush
        out.mux(packet)
    out.close()
    return buf.getvalue(), mime


# --- model pre-fetch hook (server install flow) -----------------------------
# Kokoro weights + the default voice download lazily on first synth. The install
# flow (POST /api/engines/install → POST {engineURL}/download) wants to pre-fetch
# them so the UI can show a bar. We warm the default pipeline in a background
# thread and report progress via GET /download.
_dl = {"status": "idle", "progress": 0.0, "error": None}


def _warm_default():
    try:
        pipeline_for("af_heart")  # downloads Kokoro-82M weights + the default voice
        _dl.update(status="ready", progress=1.0)
    except Exception as e:  # noqa: BLE001
        _dl.update(status="error", error=str(e))


@app.route("/download", methods=["POST"])
def download_start():
    # Already warmed (or warming) → just report current state.
    if _pipelines or _dl["status"] == "ready":
        _dl.update(status="ready", progress=1.0)
        return jsonify(_dl), 200
    if _dl["status"] != "downloading":
        _dl.update(status="downloading", progress=0.0, error=None)
        threading.Thread(target=_warm_default, daemon=True).start()
    return jsonify(_dl), 202


@app.route("/download", methods=["GET"])
def download_status():
    if _pipelines and _dl["status"] != "ready":
        _dl.update(status="ready", progress=1.0)
    return jsonify(_dl)


@app.route("/v1/models")
def models():
    return jsonify({
        "object": "list",
        "data": [{"id": "kokoro", "object": "model", "owned_by": "abookify"}],
    })


@app.route("/health")
def health():
    return jsonify({"status": "ok", "model": "kokoro", "device": DEVICE})


@app.route("/v1/audio/speech", methods=["POST"])
def speech():
    body = request.get_json(force=True, silent=True) or {}
    text = body.get("input", "")
    voice = body.get("voice") or "af_heart"
    fmt = body.get("response_format") or "mp3"
    if not text:
        return jsonify({"error": "missing input"}), 400
    try:
        audio = synth(text, voice)
        data, mime = encode(audio, fmt)
        return Response(data, mimetype=mime)
    except Exception as e:  # noqa: BLE001
        return jsonify({"error": str(e)}), 500


if __name__ == "__main__":
    from _common import resolve_host, enforce_bind_policy, install_auth

    port = int(os.environ.get("ABOOKIFY_TTS_PORT", "8880"))
    host = resolve_host()
    enforce_bind_policy(host, "tts")
    install_auth(app, "tts")
    print(f"[tts] Kokoro TTS server on {host}:{port} (device={DEVICE})")
    app.run(host=host, port=port)
