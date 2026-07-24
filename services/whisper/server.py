"""Faster-whisper STT HTTP service."""
import os
import tempfile
import json
from flask import Flask, request, jsonify

from faster_whisper import WhisperModel

app = Flask(__name__)

MODEL_SIZE = os.environ.get("WHISPER_MODEL", "small")
# WHISPER_DEVICE may be "cpu", "cuda", or "auto" (default). "auto" probes for a
# usable CUDA device and otherwise runs on CPU — so a GPU-configured deploy whose
# driver is missing/mismatched degrades to CPU instead of hard-failing at startup.
REQUESTED_DEVICE = os.environ.get("WHISPER_DEVICE", "auto")
REQUESTED_COMPUTE = os.environ.get("WHISPER_COMPUTE_TYPE", "")


def _cuda_available():
    """True if faster-whisper's CTranslate2 backend can see a CUDA device."""
    try:
        import ctranslate2
        return ctranslate2.get_cuda_device_count() > 0
    except Exception as e:
        print(f"CUDA probe failed ({e}); assuming no GPU")
        return False


def _default_compute(device):
    return "float16" if device == "cuda" else "int8"


def _load_model():
    """Resolve the device (honoring WHISPER_DEVICE / auto-detect), load the
    model, and fall back to CPU int8 if a CUDA load fails. Returns
    (model, device, compute_type)."""
    dev = REQUESTED_DEVICE.strip().lower() or "auto"
    if dev == "auto":
        dev = "cuda" if _cuda_available() else "cpu"
    comp = REQUESTED_COMPUTE.strip() or _default_compute(dev)
    print(f"Loading Whisper model: {MODEL_SIZE} (device={dev}, compute={comp})")
    try:
        return WhisperModel(MODEL_SIZE, device=dev, compute_type=comp), dev, comp
    except Exception as e:
        if dev == "cuda":
            print(f"CUDA model load failed ({e}); falling back to CPU int8")
            comp = REQUESTED_COMPUTE.strip() or "int8"
            return WhisperModel(MODEL_SIZE, device="cpu", compute_type=comp), "cpu", comp
        raise


model, DEVICE, COMPUTE_TYPE = _load_model()
print(f"Model loaded (device={DEVICE}, compute={COMPUTE_TYPE}).")


@app.route("/health")
def health():
    return jsonify({
        "status": "ok",
        "model": MODEL_SIZE,
        "device": DEVICE,
        "compute_type": COMPUTE_TYPE,
        "gpu_available": DEVICE == "cuda",
    })


@app.route("/transcribe", methods=["POST"])
def transcribe():
    """Transcribe an audio file.

    POST multipart/form-data with 'file' field.
    Optional query params: language, word_timestamps (true/false)

    Returns JSON with segments and optional word-level timestamps.
    """
    if "file" not in request.files:
        return jsonify({"error": "missing file field"}), 400

    audio_file = request.files["file"]
    language = request.form.get("language")
    word_timestamps = request.form.get("word_timestamps", "true").lower() == "true"
    # initial_prompt seeds Whisper's decoder with vocabulary/spelling hints
    # (proper nouns, foreign terms) so they're more likely to be emitted
    # verbatim. Whisper truncates internally to the last 224 BPE tokens.
    initial_prompt = request.form.get("initial_prompt") or None

    # Save uploaded file temporarily
    with tempfile.NamedTemporaryFile(suffix=".audio", delete=False) as tmp:
        audio_file.save(tmp)
        tmp_path = tmp.name

    try:
        segments, info = model.transcribe(
            tmp_path,
            language=language if language else None,
            word_timestamps=word_timestamps,
            vad_filter=True,
            initial_prompt=initial_prompt,
        )

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

    except Exception as e:
        return jsonify({"error": str(e)}), 500

    finally:
        try:
            os.unlink(tmp_path)
        except OSError:
            pass


if __name__ == "__main__":
    print(f"Whisper STT server starting (model: {MODEL_SIZE})")
    app.run(host="0.0.0.0", port=5200)
