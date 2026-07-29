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


def _run_transcribe(path, language, word_timestamps, initial_prompt, vad_filter,
                    condition_on_previous_text=True):
    """One transcribe pass, fully materialized.

    faster-whisper returns a LAZY generator, so decode errors surface while
    iterating, not at the call — the list() is what makes a failure catchable.
    """
    segments, info = model.transcribe(
        path,
        language=language if language else None,
        word_timestamps=word_timestamps,
        vad_filter=vad_filter,
        initial_prompt=initial_prompt,
        condition_on_previous_text=condition_on_previous_text,
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

    return info, result_segments, full_text_parts


def _looks_looped(segments):
    """True when the output shows Whisper's repetition-loop signature.

    Conditioning on previously generated text can trap the decoder: it emits the
    same short phrase over and over instead of transcribing, and because it
    RETURNS NORMALLY nothing downstream notices. Pride and Prejudice lost ~550
    words of one chunk to 8 repetitions of "CHAPTER VII." spread over 210
    seconds, with confidence collapsing 0.98 -> 0.02 as it went. Re-running
    reproduced it exactly, because the failure is deterministic.

    Detected structurally rather than by confidence: a loop repeats identical
    segment text many times while covering a long stretch of audio. Ordinary
    narration repeats a whole segment only rarely, and short recordings are
    exempt so a genuine refrain in a 3-segment clip cannot trip it.
    """
    texts = [s["text"].strip() for s in segments if s.get("text", "").strip()]
    if len(texts) < 8:
        return False
    counts = {}
    for t in texts:
        counts[t] = counts.get(t, 0) + 1
    most = max(counts.values())
    # A third of all segments being one identical string is not narration.
    return most >= 4 and most / len(texts) >= 0.33


def _span_seconds(segments):
    """Audio actually covered by the returned segments."""
    if not segments:
        return 0.0
    return max(s["end"] for s in segments) - min(s["start"] for s in segments)


def _transcribe_degrading(path, language, word_timestamps, initial_prompt, vad_filter=True,
                          condition_prev=True):
    """Transcribe, stepping down one capability at a time on a model crash.

    faster-whisper can hard-fail on a specific chunk — most often
    "boolean index did not match indexed array ..." raised from the
    word-alignment pass when VAD leaves it an empty speech array. That failure
    is DETERMINISTIC, so the client's retry/backoff loop can never clear it: the
    caller burns its attempts and drops the chunk, silently losing that stretch
    of a book.

    So degrade instead of failing, keeping the most valuable capability longest
    (word timings drive karaoke, so VAD is dropped before they are). Returns
    (info, segments, text_parts, degraded) where degraded names the fallback
    that worked, or None when the normal path did.
    """
    ladder = [
        (None, vad_filter, word_timestamps),
    ]
    if vad_filter:
        ladder.append(("no_vad", False, word_timestamps))
    if word_timestamps:
        # Last resort: segment-level times only. Costs word-level karaoke for
        # this chunk, but keeps its text and coarse timings.
        ladder.append(("no_vad_no_word_timestamps", False, False))

    last_err = None
    for degraded, vad, words in ladder:
        try:
            info, segs, parts = _run_transcribe(
                path, language, words, initial_prompt, vad, condition_prev)

            # A repetition loop is not an exception — the call SUCCEEDS and
            # returns a plausible-looking result that is mostly one repeated
            # phrase. Retry once without conditioning, which is what traps the
            # decoder, and keep whichever pass transcribed more of the audio.
            if condition_prev and _looks_looped(segs):
                print(f"transcribe: repetition loop detected "
                      f"({len(segs)} segments); retrying without "
                      f"condition_on_previous_text", flush=True)
                try:
                    info2, segs2, parts2 = _run_transcribe(
                        path, language, words, initial_prompt, vad, False)
                    if len(" ".join(parts2).split()) > len(" ".join(parts).split()):
                        print(f"transcribe: recovered from loop "
                              f"({len(' '.join(parts).split())} -> "
                              f"{len(' '.join(parts2).split())} words)", flush=True)
                        tag = "no_condition_prev"
                        if degraded:
                            tag = degraded + "+no_condition_prev"
                        return info2, segs2, parts2, tag
                    print("transcribe: retry did not improve; keeping first pass",
                          flush=True)
                except Exception as e:
                    print(f"transcribe: loop retry failed ({e}); keeping first pass",
                          flush=True)

            if degraded:
                print(f"transcribe: recovered via {degraded} "
                      f"(after: {last_err})", flush=True)
            return info, segs, parts, degraded
        except Exception as e:
            last_err = e
            print(f"transcribe: pass "
                  f"(vad={vad}, word_timestamps={words}) failed: {e}", flush=True)
    raise last_err


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
    # vad_filter defaults to on (unchanged behaviour). Callers can disable it for
    # recordings where the VAD discards real speech — it silently drops whole
    # stretches on amateur/variable-level audio rather than erroring, which is
    # invisible in the output.
    cond_param = request.form.get("condition_on_previous_text")
    condition_prev = True if cond_param is None else cond_param.lower() not in ("false", "0", "no")
    vad_param = request.form.get("vad_filter")
    vad_filter = True if vad_param is None else vad_param.lower() not in ("false", "0", "no")

    # Save uploaded file temporarily
    with tempfile.NamedTemporaryFile(suffix=".audio", delete=False) as tmp:
        audio_file.save(tmp)
        tmp_path = tmp.name

    try:
        info, result_segments, full_text_parts, degraded = _transcribe_degrading(
            tmp_path, language, word_timestamps, initial_prompt, vad_filter, condition_prev)

        body = {
            "language": info.language,
            "language_probability": round(info.language_probability, 3),
            "duration": round(info.duration, 3),
            "text": " ".join(full_text_parts),
            "segments": result_segments,
        }
        if degraded:
            body["degraded"] = degraded
        return jsonify(body)

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
