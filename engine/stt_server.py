"""Faster-whisper STT HTTP service (hermetic-engine edition).

Same wire contract as services/whisper/server.py — the Go server
(internal/stt) and stt-cli already speak it, so this must stay byte-compatible:

  GET  /health      -> {"status","model","device","compute_type"}
  POST /transcribe  multipart: file=<audio>, form: language, word_timestamps,
                    initial_prompt  -> {language, language_probability, duration,
                    text, segments:[{start,end,text,words:[{word,start,end,
                    probability}]}]}

Difference vs the Docker service: device/compute_type are AUTO-DETECTED at
startup (CUDA -> float16, else CPU int8) unless overridden by env. Models live
under ~/.abookify/models/whisper so the bundle never writes into site-packages.
"""
import os
import gc
import time
import tempfile
from pathlib import Path

from flask import Flask, request, jsonify

from _common import gpu_lock

import ctranslate2
from faster_whisper import WhisperModel

app = Flask(__name__)

# Idle unloading (parity with services/whisper/server.py): the ~3 GB model can be
# freed when nothing is transcribing and reloaded transparently on the next
# request. The Go server drives /unload via the same stt_idle_timeout setting, so
# ONE control governs both the Docker and hermetic-engine paths. LAST_LOAD_SECS
# records the reload (cold-start) cost for /health.
LAST_LOAD_SECS = 0.0

MODELS_DIR = Path(
    os.environ.get("ABOOKIFY_MODELS_DIR", Path.home() / ".abookify" / "models")
)
WHISPER_CACHE = MODELS_DIR / "whisper"
WHISPER_CACHE.mkdir(parents=True, exist_ok=True)


def detect_device():
    """Pick (device, compute_type). Env overrides win; else auto-detect CUDA.

    ABOOKIFY_DEVICE / ABOOKIFY_COMPUTE_TYPE override (also honours the legacy
    WHISPER_DEVICE / WHISPER_COMPUTE_TYPE the Docker image used).
    """
    dev = os.environ.get("ABOOKIFY_DEVICE") or os.environ.get("WHISPER_DEVICE")
    comp = os.environ.get("ABOOKIFY_COMPUTE_TYPE") or os.environ.get("WHISPER_COMPUTE_TYPE")
    if dev:
        if not comp:
            comp = "float16" if dev == "cuda" else "int8"
        return dev, comp
    try:
        if ctranslate2.get_cuda_device_count() > 0:
            return "cuda", comp or "float16"
    except Exception as e:  # noqa: BLE001 - any CUDA probe failure -> CPU
        print(f"[stt] CUDA probe failed ({e}); falling back to CPU")
    return "cpu", comp or "int8"


MODEL_SIZE = os.environ.get("ABOOKIFY_WHISPER_MODEL") or os.environ.get("WHISPER_MODEL", "large-v3")
DEVICE, COMPUTE_TYPE = detect_device()

print(f"[stt] Loading Whisper {MODEL_SIZE} (device={DEVICE}, compute={COMPUTE_TYPE})")
print(f"[stt] model cache: {WHISPER_CACHE}")
try:
    model = WhisperModel(
        MODEL_SIZE, device=DEVICE, compute_type=COMPUTE_TYPE, download_root=str(WHISPER_CACHE)
    )
except Exception as e:  # noqa: BLE001 - GPU init can fail at runtime; degrade
    if DEVICE == "cuda":
        print(f"[stt] CUDA model load failed ({e}); retrying on CPU int8")
        DEVICE, COMPUTE_TYPE = "cpu", "int8"
        model = WhisperModel(
            MODEL_SIZE, device=DEVICE, compute_type=COMPUTE_TYPE, download_root=str(WHISPER_CACHE)
        )
    else:
        raise
print(f"[stt] Model loaded on {DEVICE}.")


def _load_if_needed_locked():
    """Reload the model if it was idle-unloaded. The GPU lock MUST be held (the
    caller already holds gpu_lock('stt')). Records the cold-start cost."""
    global model, LAST_LOAD_SECS
    if model is not None:
        return
    t = time.perf_counter()
    print(f"[stt] reloading Whisper after idle unload (device={DEVICE}, compute={COMPUTE_TYPE})")
    model = WhisperModel(
        MODEL_SIZE, device=DEVICE, compute_type=COMPUTE_TYPE, download_root=str(WHISPER_CACHE)
    )
    LAST_LOAD_SECS = time.perf_counter() - t
    print(f"[stt] model reloaded in {LAST_LOAD_SECS:.2f}s")


@app.route("/health")
def health():
    return jsonify({
        "status": "ok",
        "model": MODEL_SIZE,
        "device": DEVICE,
        "compute_type": COMPUTE_TYPE,
        "loaded": model is not None,
        "last_load_secs": round(LAST_LOAD_SECS, 2),
    })


@app.route("/unload", methods=["POST"])
def unload():
    """Free the Whisper model from RAM/VRAM (idle unloading). The shared GPU lock
    makes this wait for any in-flight STT/TTS inference, so it never interrupts a
    running job; the Go server only calls it when no job is queued anyway. The
    next /transcribe reloads transparently."""
    global model
    with gpu_lock("stt"):
        was_loaded = model is not None
        model = None
        gc.collect()
    if was_loaded:
        print("[stt] model unloaded (idle).")
    return jsonify({"unloaded": was_loaded})


@app.route("/download", methods=["POST", "GET"])
def download():
    """Model pre-fetch hook for the server's install flow (POST /api/engines/install
    → POST {engineURL}/download). The Whisper model is fetched + loaded at startup
    (WhisperModel init blocks until it's local), so by the time this service is
    answering at all, the model is already present — always report ready.
    """
    return jsonify({"status": "ready", "progress": 1.0, "model": MODEL_SIZE})


@app.route("/transcribe", methods=["POST"])
def transcribe():
    if "file" not in request.files:
        return jsonify({"error": "missing file field"}), 400

    audio_file = request.files["file"]
    language = request.form.get("language")
    word_timestamps = request.form.get("word_timestamps", "true").lower() == "true"
    initial_prompt = request.form.get("initial_prompt") or None

    with tempfile.NamedTemporaryFile(suffix=".audio", delete=False) as tmp:
        audio_file.save(tmp)
        tmp_path = tmp.name

    try:
        # Serialize GPU inference across all callers (#132): the engine is the
        # single GPU dispatcher. faster-whisper is lazy — the GPU work happens
        # while the generator is consumed — so we materialize it under the lock.
        with gpu_lock("stt"):
            _load_if_needed_locked()  # transparent reload if idle-unloaded
            segments, info = model.transcribe(
                tmp_path,
                language=language if language else None,
                word_timestamps=word_timestamps,
                vad_filter=True,
                initial_prompt=initial_prompt,
            )
            segments = list(segments)

        result_segments = []
        full_text_parts = []
        for segment in segments:
            seg_data = {
                "start": round(segment.start, 3),
                "end": round(segment.end, 3),
                "text": segment.text.strip(),
            }
            if word_timestamps and segment.words:
                seg_data["words"] = [
                    {
                        "word": w.word,
                        "start": round(w.start, 3),
                        "end": round(w.end, 3),
                        "probability": round(w.probability, 3),
                    }
                    for w in segment.words
                ]
            result_segments.append(seg_data)
            full_text_parts.append(segment.text.strip())

        return jsonify({
            "language": info.language,
            "language_probability": round(info.language_probability, 3),
            "duration": round(info.duration, 3),
            "text": " ".join(full_text_parts),
            "segments": result_segments,
        })
    except Exception as e:  # noqa: BLE001 - mirror Docker service's error contract
        return jsonify({"error": str(e)}), 500
    finally:
        try:
            os.unlink(tmp_path)
        except OSError:
            pass


if __name__ == "__main__":
    from _common import resolve_host, enforce_bind_policy, install_auth

    port = int(os.environ.get("ABOOKIFY_STT_PORT", "5200"))
    host = resolve_host()
    enforce_bind_policy(host, "stt")
    install_auth(app, "stt")
    print(f"[stt] Whisper STT server on {host}:{port} (model={MODEL_SIZE}, device={DEVICE})")
    app.run(host=host, port=port)
