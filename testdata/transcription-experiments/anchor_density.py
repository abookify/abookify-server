#!/usr/bin/env python3
"""Measure anchor density between an ebook and its Whisper transcript.

For the anchor-based forced-alignment design (see
docs/epub-informed-transcription.md): the recursive aligner finds short
word-sequences that are unique in the ebook and locate-able in the
transcript, uses them as anchors, and recurses into the gaps. This script
measures, on real imported works, how dense and unambiguous those anchors
actually are at n-gram lengths 3-6.

Read-only against the local SQLite DB. Usage:
    python3 anchor_density.py            # works 27 (KC) + 28 (Frankenstein)
"""
import sqlite3, re, sys
from collections import Counter

DB = "engineering/server/data/abookify.db"

def text_for(db, work_id, origin):
    rows = db.execute(
        "SELECT c.content FROM chapters c JOIN books b ON b.id=c.book_id "
        "WHERE b.work_id=? AND b.origin=? ORDER BY c.index_num",
        (work_id, origin)).fetchall()
    return " ".join(r[0] or "" for r in rows)

def norm(t):
    t = t.lower().replace("’", "'")
    t = re.sub(r"[^a-z0-9' ]", " ", t)
    return [w for w in t.split() if w]

def grams(toks, n):
    return [" ".join(toks[i:i+n]) for i in range(len(toks)-n+1)]

def analyze(db, work_id, label):
    eb = norm(text_for(db, work_id, "publisher_epub"))
    tr = norm(text_for(db, work_id, "whisper_transcript"))
    print(f"\n===== {label} (work {work_id}) =====  "
          f"ebook {len(eb):,}w  transcript {len(tr):,}w")
    for n in (3, 4, 5, 6):
        eg = grams(eb, n)
        ec = Counter(eg)
        tc = Counter(grams(tr, n))
        eb_hapax = {g for g, c in ec.items() if c == 1}
        clean = {g for g in eb_hapax if tc.get(g, 0) == 1}      # unambiguous 1:1
        ambig = sum(1 for g in eb_hapax if tc.get(g, 0) > 1)     # 1 in ebook, N in transcript
        multi_both = sum(1 for g, c in ec.items() if c > 1 and tc.get(g, 0) > 1)
        dens = 1000 * len(clean) / max(1, len(eb))
        pos = [i for i, g in enumerate(eg) if g in clean]
        gaps = sorted(pos[i+1]-pos[i] for i in range(len(pos)-1)) if len(pos) > 1 else []
        med = gaps[len(gaps)//2] if gaps else 0
        p95 = gaps[int(len(gaps)*0.95)] if gaps else 0
        mx = gaps[-1] if gaps else 0
        print(f"  n={n}: hapax {len(eb_hapax):>6,} | clean 1:1 {len(clean):>6,} "
              f"({dens:4.1f}/1kw  gap med {med:>3} p95 {p95:>4} max {mx:>5}) "
              f"| 1:N {ambig:>5,} | multi-both {multi_both:>5,}")

if __name__ == "__main__":
    db = sqlite3.connect(f"file:{DB}?mode=ro", uri=True)
    analyze(db, 27, "Kitchen Confidential")
    analyze(db, 28, "Frankenstein")
