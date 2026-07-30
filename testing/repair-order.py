#!/usr/bin/env python3
"""Emit the repair order for the whole library, worst first.

Worst-first so the largest correctness win lands earliest and the run can be
stopped at any point with the remainder being the least affected books.

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
        try:
            words = json.load(open(sc))["words"]
        except Exception as e:
            print(f"# skip {title}: unreadable sidecar ({e})", file=sys.stderr)
            continue
        collapsed, lowconf = measure(words)
        audiodir = os.path.dirname(host) if len(audio) > 1 else ""
        rows.append((collapsed, lowconf, int(dur), sc, audiodir, wid, title))

    rows.sort(key=lambda r: -r[0])
    total_h = sum(r[2] for r in rows) / 3600
    print(f"# {len(rows)} book(s), {total_h:.1f} h audio, "
          f"{sum(r[0] for r in rows):,} collapsed words", file=sys.stderr)
    for collapsed, lowconf, dur, sc, audiodir, wid, title in rows:
        print(f"{collapsed}\t{dur}\t{sc}\t{audiodir}\t{wid}")


if __name__ == "__main__":
    main()
