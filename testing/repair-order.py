#!/usr/bin/env python3
"""Emit the repair order for the whole library, best-yield first.

Ordered by fabricated words fixed per GPU HOUR, not by fabricated words. Those
are different orderings and the difference is not academic: Lord of the Rings
holds the most invented text in the library (9,965 words) and is also its longest
book (52 h), so worst-first spends ~7 GPU hours before a single book completes.
Ordering by yield puts several short books first — within an hour there are three
finished books with real before/after numbers.

That matters for more than impatience. A long first book means a long blind
window in which nothing has been proven end to end, and this project has been
bitten repeatedly by the gap between "it ran" and "it worked". Finishing small
books early converts that from a hope into a measurement.

Stopping is then free at any point, because every hour spent was the best hour
available at the time.

RESUMED BOOKS ARE RANKED BY WHAT IS LEFT. A book part-way through a previous run
has already banked those files (they are checkpointed per file), so charging it
for its whole duration would push it down the order for work it no longer has to
do. Completed files are discounted using the durations in its own sidecar.

"Worst" is the collapsed-timestamp count: words sharing a timestamp with more
than six others. That is the fabrication signature — timings that were
synthesized rather than measured, and in this library those spans have
repeatedly turned out not to match the audio. Low-confidence words are printed
alongside as context but do NOT drive the ordering: they are evidence of hard
audio as often as of invention.

Reads sidecars from disk rather than the database on purpose. The sidecar is
what a re-transcription rewrites, so it is the artefact whose state decides
whether a book needs GPU spent on it.

    python3 testing/repair-order.py > order.tsv
    columns: collapsed_words, duration_secs, sidecar_path, audio_dir_or_empty, work_id
"""
import json
import os
import sqlite3
import sys
from collections import Counter

DB = os.environ.get("REPAIR_DB", "./data/abookify.db")
LIB = os.environ.get("REPAIR_LIB", "testdata/library")
# Per-file checkpoint ledger, so a resumed book is costed by its remaining files.
FILEDONE = os.environ.get("REPAIR_FILEDONE", "")
# Pre-repair backups. MEASURE THESE, NOT THE LIVE SIDECAR, when they exist: a book
# interrupted mid-repair has a sidecar holding some rebuilt files and some empty
# ones, so counting it live reports a fraction of the damage and ranks the book as
# nearly clean. Lord of the Rings read as 53 collapsed words instead of 9,965 that
# way — the same mistake as measuring a resumed book's "before" from its partially
# rebuilt state, which is a bug this run already had once.
BACKUPS = os.environ.get("REPAIR_BACKUPS", "")
# Measured throughput on this GPU (RTX 3060, large-v3, float16).
REALTIME = 6.2


def banked_seconds(name, sidecar):
    """Seconds of audio already transcribed for this book in a previous run."""
    if not FILEDONE or not os.path.exists(FILEDONE):
        return 0.0
    done = set()
    for line in open(FILEDONE):
        parts = line.rstrip("\n").split("\t")
        if len(parts) == 3 and parts[0] == name and parts[2] != "__bootstrapped__":
            done.add(parts[2])
    if not done:
        return 0.0
    try:
        srcs = json.load(open(sidecar)).get("sources", [])
    except Exception:
        return 0.0
    return sum(s.get("duration", 0) for s in srcs if s.get("filename") in done)


def find_sidecar(host_path):
    for cand in (
        os.path.dirname(host_path) + ".stt.json",
        os.path.splitext(host_path)[0] + ".stt.json",
        host_path + ".stt.json",
    ):
        if os.path.exists(cand):
            return cand
    return None


def measure(words):
    """Collapsed-timestamp words, and words inside runs of 8+ under 0.50 conf.

    Missing 'conf' counts as 0.0, matching the Go side: the writer omits the key
    when confidence is exactly zero, so treating absence as high confidence would
    hide the worst words in the book.
    """
    counts = Counter(w["s"] for w in words)
    collapsed = sum(v for v in counts.values() if v > 6)
    lowconf, run = 0, 0
    for w in words:
        if w.get("conf", 0.0) < 0.50:
            run += 1
        else:
            if run >= 8:
                lowconf += run
            run = 0
    if run >= 8:
        lowconf += run
    return collapsed, lowconf


def main():
    con = sqlite3.connect("file:" + DB + "?mode=ro", uri=True)
    rows = []
    for wid, title in con.execute("select id,title from works order by id"):
        audio = [r[0] for r in con.execute(
            "select path from books where work_id=? and media_type='audio' order by id", (wid,))]
        if not audio:
            continue
        dur = con.execute(
            "select sum(duration) from books where work_id=? and media_type='audio'",
            (wid,)).fetchone()[0] or 0
        host = audio[0].replace("/library", LIB)
        sc = find_sidecar(host)
        if not sc:
            continue
        name = os.path.basename(sc)[:-len(".stt.json")]
        measure_from = sc
        if BACKUPS:
            bk = os.path.join(BACKUPS, name + ".stt.json")
            if os.path.exists(bk):
                measure_from = bk
        try:
            words = json.load(open(measure_from))["words"]
        except Exception as e:
            print(f"# skip {title}: unreadable sidecar ({e})", file=sys.stderr)
            continue
        collapsed, lowconf = measure(words)
        audiodir = os.path.dirname(host) if len(audio) > 1 else ""
        remaining = max(60.0, dur - banked_seconds(name, sc))
        yield_per_gpu_h = collapsed / (remaining / 3600 / REALTIME)
        rows.append((yield_per_gpu_h, collapsed, lowconf, int(dur), remaining,
                     sc, audiodir, wid, title))

    rows.sort(key=lambda r: -r[0])
    total_h = sum(r[3] for r in rows) / 3600
    print(f"# {len(rows)} book(s), {total_h:.1f} h audio, "
          f"{sum(r[1] for r in rows):,} collapsed words, "
          f"{total_h / REALTIME:.1f} GPU h", file=sys.stderr)
    print(f"# {'yield/GPUh':>10}  {'collapsed':>9}  {'GPUh':>5}  book", file=sys.stderr)
    for y, collapsed, lowconf, dur, remaining, sc, audiodir, wid, title in rows:
        print(f"#{y:10.0f}  {collapsed:9d}  {remaining / 3600 / REALTIME:5.2f}  {title[:44]}",
              file=sys.stderr)
        # "-" placeholder, never an empty field: the consumer reads with
        # IFS=$'\t', and tab is IFS *whitespace*, so consecutive tabs collapse
        # into one delimiter — an empty audiodir shifted work_id into audiodir
        # and left work_id empty, which made every single-file book transcribe
        # on GPU and then fail to land with NO_WORK_ID.
        print(f"{collapsed}\t{dur}\t{sc}\t{audiodir or '-'}\t{wid}")


if __name__ == "__main__":
    main()
