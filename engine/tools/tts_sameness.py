#!/usr/bin/env python3
"""Measure whether two Kokoro endpoints render the SAME audio for the same chunks.

Board 17 (GPU TTS path): the August rejection was a 6.8 s shortfall over a
five-minute sample between the Docker kokoro-fastapi (CPU) render and the
hermetic engine's (GPU) render. Ear says "pleasant"; only measurement says
"identical". This tool synthesizes EXACTLY the chunks the generator would send
(from the JSON that `TestDumpTTSChunks` writes) against a REFERENCE endpoint
and a CANDIDATE endpoint, decodes both as raw PCM, and compares per chunk:

  duration, leading/trailing silence, internal-gap profile (pauses ≥ gap_ms
  below thresh_db), speech duration, RMS level.

Run with the engine's own python (numpy + av are in the bundle):

  engine/dist/engine/python/bin/python3 engine/tools/tts_sameness.py \
      --chunks stave-one-chunks.json --maxsegs 10 --voice bm_fable \
      --ref http://localhost:8880 --cand http://localhost:8881 \
      --out report-dir

Outputs in --out: per-chunk TSV, a markdown summary, and the two concatenated
WAVs (with the generator's inter-segment pauses inserted, so the totals are
comparable to an assembled chapter). Nothing is judged by ear here.
"""
import argparse
import io
import json
import math
import os
import sys
import time
import urllib.request
import wave

import numpy as np

SR = 24000


def synth(url: str, text: str, voice: str, token: str | None = None) -> np.ndarray:
    body = json.dumps({"model": "kokoro", "input": text, "voice": voice, "response_format": "wav"}).encode()
    req = urllib.request.Request(url.rstrip("/") + "/v1/audio/speech", data=body,
                                 headers={"Content-Type": "application/json"})
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=1800) as r:
        data = r.read()
    return decode_wav(data)


def decode_wav(data: bytes) -> np.ndarray:
    try:
        with wave.open(io.BytesIO(data)) as w:
            assert w.getframerate() == SR, w.getframerate()
            assert w.getnchannels() == 1, w.getnchannels()
            assert w.getsampwidth() == 2, w.getsampwidth()
            pcm = np.frombuffer(w.readframes(w.getnframes()), dtype=np.int16)
        return pcm.astype(np.float32) / 32767.0
    except wave.Error:
        # kokoro-fastapi streams its WAV with a placeholder header; fall back to PyAV.
        import av
        frames = []
        with av.open(io.BytesIO(data)) as c:
            for fr in c.decode(audio=0):
                a = fr.to_ndarray()
                if a.ndim == 2:
                    a = a[0]
                if a.dtype == np.int16:
                    a = a.astype(np.float32) / 32767.0
                frames.append(a.astype(np.float32))
        return np.concatenate(frames) if frames else np.zeros(0, np.float32)


def silence_profile(x: np.ndarray, thresh_db: float, gap_ms: int, win_ms: int = 10):
    """Return (lead_s, trail_s, gaps[(start_s, dur_s)]) using a windowed RMS gate."""
    n = len(x)
    if n == 0:
        return 0.0, 0.0, []
    win = int(SR * win_ms / 1000)
    nwin = int(math.ceil(n / win))
    padded = np.zeros(nwin * win, np.float32)
    padded[:n] = x
    rms = np.sqrt(np.mean(padded.reshape(nwin, win) ** 2, axis=1))
    thr = 10 ** (thresh_db / 20)
    loud = rms > thr
    if not loud.any():
        return n / SR, 0.0, []
    first = int(np.argmax(loud))
    last = int(len(loud) - 1 - np.argmax(loud[::-1]))
    lead = first * win / SR
    trail = (n - (last + 1) * win) / SR
    gaps = []
    i = first
    min_w = max(1, int(round(gap_ms / win_ms)))
    while i <= last:
        if not loud[i]:
            j = i
            while j <= last and not loud[j]:
                j += 1
            if j - i >= min_w:
                gaps.append(((i * win) / SR, (j - i) * win / SR))
            i = j
        else:
            i += 1
    return lead, max(trail, 0.0), gaps


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--chunks", required=True, help="JSON from TestDumpTTSChunks")
    ap.add_argument("--maxsegs", type=int, default=0)
    ap.add_argument("--voice", default="bm_fable")
    ap.add_argument("--ref", required=True)
    ap.add_argument("--cand", required=True)
    ap.add_argument("--ref-token", default=os.environ.get("REF_TOKEN"))
    ap.add_argument("--cand-token", default=os.environ.get("CAND_TOKEN"))
    ap.add_argument("--out", required=True)
    ap.add_argument("--thresh-db", type=float, default=-45.0)
    ap.add_argument("--gap-ms", type=int, default=150)
    ap.add_argument("--ref-label", default="ref")
    ap.add_argument("--cand-label", default="cand")
    args = ap.parse_args()

    segs = json.load(open(args.chunks))
    if args.maxsegs > 0:
        segs = segs[: args.maxsegs]
    os.makedirs(args.out, exist_ok=True)

    rows = []
    a_all, b_all = [], []
    t_a = t_b = 0.0
    for si, seg in enumerate(segs):
        for ci, text in enumerate(seg["chunks"]):
            t0 = time.time()
            a = synth(args.ref, text, args.voice, args.ref_token)
            t_a += time.time() - t0
            t0 = time.time()
            b = synth(args.cand, text, args.voice, args.cand_token)
            t_b += time.time() - t0
            la, ta, ga = silence_profile(a, args.thresh_db, args.gap_ms)
            lb, tb, gb = silence_profile(b, args.thresh_db, args.gap_ms)
            row = {
                "seg": si, "chunk": ci, "words": len(text.split()),
                "dur_a": len(a) / SR, "dur_b": len(b) / SR,
                "lead_a": la, "lead_b": lb, "trail_a": ta, "trail_b": tb,
                "speech_a": len(a) / SR - la - ta, "speech_b": len(b) / SR - lb - tb,
                "gaps_a": len(ga), "gaps_b": len(gb),
                "gap_secs_a": sum(d for _, d in ga), "gap_secs_b": sum(d for _, d in gb),
                "rms_a": float(np.sqrt(np.mean(a ** 2))) if len(a) else 0.0,
                "rms_b": float(np.sqrt(np.mean(b ** 2))) if len(b) else 0.0,
                "text": text[:60].replace("\t", " "),
            }
            rows.append(row)
            a_all.append(a)
            b_all.append(b)
            print(f"seg {si:3d} ch {ci}: {args.ref_label} {row['dur_a']:7.3f}s  {args.cand_label} {row['dur_b']:7.3f}s  "
                  f"Δ {row['dur_b']-row['dur_a']:+6.3f}  lead {la:.3f}/{lb:.3f} trail {ta:.3f}/{tb:.3f} "
                  f"gaps {len(ga)}/{len(gb)}", flush=True)
            # honour the generator's inserted pause after the segment's last chunk
            if ci == len(seg["chunks"]) - 1 and seg.get("pause_after_ms", 0) > 0 and si < len(segs) - 1:
                z = np.zeros(int(SR * seg["pause_after_ms"] / 1000), np.float32)
                a_all.append(z)
                b_all.append(z)

    keys = ["seg", "chunk", "words", "dur_a", "dur_b", "lead_a", "lead_b", "trail_a", "trail_b",
            "speech_a", "speech_b", "gaps_a", "gaps_b", "gap_secs_a", "gap_secs_b", "rms_a", "rms_b", "text"]
    with open(os.path.join(args.out, "chunks.tsv"), "w") as f:
        f.write("\t".join(keys) + "\n")
        for r in rows:
            f.write("\t".join(f"{r[k]:.4f}" if isinstance(r[k], float) else str(r[k]) for k in keys) + "\n")

    A = np.concatenate(a_all) if a_all else np.zeros(0, np.float32)
    B = np.concatenate(b_all) if b_all else np.zeros(0, np.float32)
    for name, x in ((args.ref_label, A), (args.cand_label, B)):
        with wave.open(os.path.join(args.out, f"{name}.wav"), "w") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(SR)
            w.writeframes((np.clip(x, -1, 1) * 32767).astype(np.int16).tobytes())

    d = np.array([r["dur_b"] - r["dur_a"] for r in rows])
    sp = np.array([r["speech_b"] - r["speech_a"] for r in rows])
    lead = np.array([r["lead_b"] - r["lead_a"] for r in rows])
    trail = np.array([r["trail_b"] - r["trail_a"] for r in rows])
    gaps_a = sum(r["gaps_a"] for r in rows)
    gaps_b = sum(r["gaps_b"] for r in rows)
    gsec_a = sum(r["gap_secs_a"] for r in rows)
    gsec_b = sum(r["gap_secs_b"] for r in rows)
    n = len(rows)

    def fmt(x):
        return f"{x:+.3f}"

    md = []
    md.append(f"# TTS sameness: {args.cand_label} vs {args.ref_label}\n")
    md.append(f"chunks: {n} · voice: {args.voice} · gate {args.thresh_db:.0f} dBFS · gap ≥ {args.gap_ms} ms\n")
    md.append("| metric | ref | cand | Δ (cand − ref) |\n|---|---|---|---|")
    md.append(f"| assembled total (with pauses) | {len(A)/SR:.3f} s | {len(B)/SR:.3f} s | {fmt(len(B)/SR-len(A)/SR)} s |")
    md.append(f"| Σ chunk duration | {sum(r['dur_a'] for r in rows):.3f} s | {sum(r['dur_b'] for r in rows):.3f} s | {fmt(d.sum())} s |")
    md.append(f"| Σ speech (dur − lead − trail) | {sum(r['speech_a'] for r in rows):.3f} s | {sum(r['speech_b'] for r in rows):.3f} s | {fmt(sp.sum())} s |")
    md.append(f"| Σ leading silence | {sum(r['lead_a'] for r in rows):.3f} s | {sum(r['lead_b'] for r in rows):.3f} s | {fmt(lead.sum())} s |")
    md.append(f"| Σ trailing silence | {sum(r['trail_a'] for r in rows):.3f} s | {sum(r['trail_b'] for r in rows):.3f} s | {fmt(trail.sum())} s |")
    md.append(f"| internal gaps (count) | {gaps_a} | {gaps_b} | {gaps_b-gaps_a:+d} |")
    md.append(f"| internal gaps (Σ secs) | {gsec_a:.3f} s | {gsec_b:.3f} s | {fmt(gsec_b-gsec_a)} s |")
    md.append(f"| per-chunk Δ duration | | | mean {fmt(d.mean() if n else 0)} · max |Δ| {abs(d).max() if n else 0:.3f} · p95 |Δ| {np.percentile(abs(d),95) if n else 0:.3f} |")
    md.append(f"| per-chunk Δ speech | | | mean {fmt(sp.mean() if n else 0)} · max |Δ| {abs(sp).max() if n else 0:.3f} |")
    md.append(f"| per-chunk Δ lead / trail (mean) | | | {fmt(lead.mean() if n else 0)} / {fmt(trail.mean() if n else 0)} |")
    md.append(f"| wall time | {t_a:.1f} s | {t_b:.1f} s | {t_a/max(t_b,1e-9):.1f}× faster |\n")
    worst = sorted(rows, key=lambda r: -abs(r["dur_b"] - r["dur_a"]))[:8]
    md.append("Worst chunks by |Δ duration|:\n")
    md.append("| seg | words | ref | cand | Δ | Δ speech | Δ lead | Δ trail | gaps ref/cand | text |\n|---|---|---|---|---|---|---|---|---|---|")
    for r in worst:
        md.append(f"| {r['seg']} | {r['words']} | {r['dur_a']:.3f} | {r['dur_b']:.3f} | {fmt(r['dur_b']-r['dur_a'])} | "
                  f"{fmt(r['speech_b']-r['speech_a'])} | {fmt(r['lead_b']-r['lead_a'])} | {fmt(r['trail_b']-r['trail_a'])} | "
                  f"{r['gaps_a']}/{r['gaps_b']} | {r['text']} |")
    report = "\n".join(md) + "\n"
    open(os.path.join(args.out, "report.md"), "w").write(report)
    print("\n" + report)


if __name__ == "__main__":
    main()
