"""Faster-whisper STT HTTP service."""
import gc
import os
import threading
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


# RLock, not Lock: _run_transcribe holds it across the whole decode and calls
# _get_model() inside, which re-acquires. A plain Lock deadlocks there.
#
# Held across the ENTIRE transcribe, including the lazy-generator
# materialization — not merely around fetching the model. That is what makes
# /unload safe: an unload cannot free the weights out from under a decode in
# progress, and concurrent decodes through the single WhisperModel are
# serialized. This restores protection that server-web had built (MODEL_LOCK)
# and that I destroyed in 9316e34 by writing this file from a tree state that
# predated their work.
_model_lock = threading.RLock()
model, DEVICE, COMPUTE_TYPE = _load_model()
print(f"Model loaded (device={DEVICE}, compute={COMPUTE_TYPE}).")


def _get_model():
    """Return the loaded model, reloading transparently after an unload.

    Callers never see the difference: an unloaded service reloads on the next
    transcribe, costing roughly two seconds on this hardware.
    """
    global model, DEVICE, COMPUTE_TYPE
    with _model_lock:
        if model is None:
            print("Model not resident; reloading on demand...", flush=True)
            model, DEVICE, COMPUTE_TYPE = _load_model()
        return model


@app.route("/health")
def health():
    return jsonify({
        "status": "ok",
        "model": MODEL_SIZE,
        "device": DEVICE,
        "compute_type": COMPUTE_TYPE,
        "gpu_available": DEVICE == "cuda",
        # Whether the weights are actually resident. The idle-unload setting is
        # meaningless without this: a caller cannot otherwise tell whether the
        # memory it thinks it freed is really free.
        "model_loaded": model is not None,
    })


@app.route("/unload", methods=["POST"])
def unload():
    """Drop the model and release its memory (~3 GB, VRAM on GPU).

    Exists because the product exposes an "Unload from memory after idle"
    setting that, on the Docker stack, had nothing to call — the endpoint was
    documented as shipped but was never implemented, so the setting silently
    reclaimed nothing. Reporting a freed 3 GB that is still allocated is worse
    than not offering the option.

    Idempotent: unloading an already-unloaded service is a no-op success. The
    next transcribe reloads transparently.
    """
    global model
    with _model_lock:
        was_loaded = model is not None
        if was_loaded:
            model = None
            # ctranslate2 frees its device memory when the last reference goes;
            # the collection is what makes that happen promptly rather than at
            # some arbitrary later point.
            gc.collect()
            print("Model unloaded on request; memory released.", flush=True)
    return jsonify({
        "status": "ok",
        "was_loaded": was_loaded,
        "model_loaded": False,
        "device": DEVICE,
    })


def _run_transcribe(path, language, word_timestamps, initial_prompt, vad_filter,
                    condition_on_previous_text=True):
    """One transcribe pass, fully materialized.

    faster-whisper returns a LAZY generator, so decode errors surface while
    iterating, not at the call — the list() is what makes a failure catchable.
    """
    with _model_lock:
        return _transcribe_locked(path, language, word_timestamps, initial_prompt,
                                  vad_filter, condition_on_previous_text)


def _transcribe_locked(path, language, word_timestamps, initial_prompt, vad_filter,
                       condition_on_previous_text=True):
    """The decode itself. _model_lock MUST be held for the whole call, so an
    /unload cannot free the model mid-decode."""
    segments, info = _get_model().transcribe(
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

    Keyed on the TIME the repetition spans, not on its share of the segments.
    That distinction is the whole detector: P&P's loop is 8 segments out of
    roughly 100 — under 10% — because 210 seconds of looping is followed by 390
    seconds of ordinary narration in the same chunk. A share-based threshold
    misses it completely, which is exactly what a first attempt keyed on share
    did. What makes it pathological is that one short phrase covers 200 seconds
    of audio, and nothing legitimate does that.

    Requires the phrase to be short, since loops latch onto fragments rather than
    sentences, and requires several repetitions so an incidental echo is ignored.
    A false positive costs one extra pass and cannot lose words, because the
    caller keeps whichever pass transcribed more.
    """
    spans = {}
    for s in segments:
        t = s.get("text", "").strip()
        if not t or len(t) > 60:
            continue
        first, last, n = spans.get(t, (s["start"], s["end"], 0))
        spans[t] = (min(first, s["start"]), max(last, s["end"]), n + 1)

    for t, (first, last, n) in spans.items():
        if n >= 4 and (last - first) >= 60.0:
            return True
    return _looks_stuttered(segments) or _looks_degenerate(segments)


def _looks_degenerate(segments):
    """True when a segment's word timings are degenerate — many words claiming
    one instant.

    faster-whisper's word-alignment pass can collapse, assigning every word in a
    segment the segment's own start time. When it does, the TEXT that comes with
    it is not what the audio says. Atlas Shrugged's residue after the stutter fix
    was entirely this, including obvious model failure like Greek and Korean
    characters mid-sentence:

        "grass as the lights\u03bc\u03ac and tr\ud558\uac8c\uc2b5\ub2c8\ub2e4 of everything were cut off"
        audio: "torch lighted over the snow on the ridges of wyatt oil"

    This is a DIFFERENT failure from the repetition loop: there is no repeated
    phrase to key on, so _looks_stuttered cannot see it. The collapsed timings are
    the only reliable signal, and they are the same signal the write-time
    integrity check uses on the finished sidecar — this just catches it early
    enough to retry.
    """
    for s in segments:
        words = s.get("words") or []
        if len(words) <= maxWordsPerInstant:
            continue
        counts = {}
        for w in words:
            counts[w["start"]] = counts.get(w["start"], 0) + 1
            if counts[w["start"]] > maxWordsPerInstant:
                return True
    return False


# Real word timings are never identical across a run of words; a handful of ties
# is normal rounding. Matches the threshold the sidecar integrity check uses.
maxWordsPerInstant = 6


def _looks_stuttered(segments):
    """True when a segment simply repeats what it just said.

    The sustained loop above is one scale of the same defect; this is the other.
    Frankenstein's fresh transcription produced "I am not a man, and I am not a
    woman. I am not a man, and I am not a woman." where the audio says
    "temperature of this place is not fitting to your fine sensations" — a clause
    emitted twice back to back, over ~20 seconds. Two repetitions inside 20s clear
    neither the >=4-times nor the >=60s bar, so the sustained-loop detector never
    fires, and the result is fabricated text with collapsed word timings that
    reads as a successful transcription.

    Detected two ways, because the stutter shows up at both granularities:

      - a segment whose text exactly repeats the previous segment's;
      - a segment that says the same phrase twice within itself.

    The second is checked as a repeated n-gram rather than an even split, because
    the repeat is often TRUNCATED or has stray words wedged between the copies:

        "I looked on the valley before me, and saw that there was nothing there.
         I looked on the valley before me, and saw that there was"

    An even-split test misses that; a repeated 6-word sequence catches it. Six is
    long enough that ordinary prose does not repeat one by accident inside a
    single ~25-word segment, so a refrain, a repeated name or "No, no." cannot
    trip it.
    """
    MIN_WORDS = 6

    prev = ""
    for s in segments:
        t = " ".join(s.get("text", "").strip().lower().split())
        if not t:
            continue
        words = t.split()

        # Consecutive segments carrying the same sentence.
        if t == prev and len(words) >= MIN_WORDS:
            return True
        prev = t

        # The same run of words said twice inside one segment.
        if len(words) >= MIN_WORDS * 2:
            seen = set()
            for i in range(len(words) - MIN_WORDS + 1):
                gram = tuple(words[i:i + MIN_WORDS])
                if gram in seen:
                    return True
                seen.add(gram)
    return False


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
                    n1 = len(" ".join(parts).split())
                    n2 = len(" ".join(parts2).split())
                    # Prefer the pass that is CLEAN over the pass that is LONGER.
                    # Selecting on word count alone is how a bad retry shipped:
                    # Heart Goes Last's chunk came back with more words that were
                    # fabricated, and every number read as a successful recovery.
                    # A higher word count is not evidence of correctness.
                    retry_clean = not _looks_looped(segs2)
                    if retry_clean or n2 > n1:
                        why = "clean" if retry_clean else "longer but still looped"
                        print(f"transcribe: recovered from loop "
                              f"({n1} -> {n2} words, {why})", flush=True)
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
