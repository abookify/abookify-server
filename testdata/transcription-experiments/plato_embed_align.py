#!/usr/bin/env python3
"""Prototype: semantic (embedding) alignment of two TRANSLATIONS of the same
work — Plato's Republic audiobook transcript (modern translation) vs the
Jowett Republic text. Lexical anchoring scored 0.2% here; this tests whether
paragraph embeddings + nearest-neighbor matching recover the correspondence.

Runs on atrium (Ollama nomic-embed-text at localhost:11434). Read-only.
"""
import json, urllib.request, sys, math, time

OLLAMA = "http://localhost:11434/api/embeddings"
MODEL = "nomic-embed-text"
CHUNK_WORDS = 120

def embed(text):
    body = json.dumps({"model": MODEL, "prompt": "search_document: " + text}).encode()
    req = urllib.request.Request(OLLAMA, data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.load(r)["embedding"]

def chunks(words, n):
    return [" ".join(words[i:i+n]) for i in range(0, len(words), n)]

def norm(v):
    s = math.sqrt(sum(x*x for x in v)) or 1.0
    return [x/s for x in v]

def embed_all(label, words):
    cs = chunks(words, CHUNK_WORDS)
    out = []
    t0 = time.time()
    for i, c in enumerate(cs):
        out.append(norm(embed(c)))
        if i % 200 == 0:
            print(f"  {label}: embedded {i}/{len(cs)} ({time.time()-t0:.0f}s)", flush=True)
    print(f"  {label}: {len(cs)} chunks embedded in {time.time()-t0:.0f}s", flush=True)
    return cs, out

def main():
    tr_words = open("/tmp/plato-embed/transcript.txt").read().split()
    jo_words = open("/tmp/plato-embed/jowett_republic.txt").read().split()
    print(f"transcript {len(tr_words)} words, jowett {len(jo_words)} words, chunk={CHUNK_WORDS}")

    tr_chunks, tr_emb = embed_all("transcript", tr_words)
    jo_chunks, jo_emb = embed_all("jowett", jo_words)

    # For each transcript chunk, find best-matching jowett chunk (max cosine).
    best_idx, best_sim = [], []
    for te in tr_emb:
        bi, bs = -1, -2.0
        for j, je in enumerate(jo_emb):
            s = sum(a*b for a, b in zip(te, je))  # cosine (both normalized)
            if s > bs:
                bs, bi = s, j
        best_idx.append(bi); best_sim.append(bs)

    sims = sorted(best_sim)
    n = len(sims)
    print(f"\nbest-match cosine: min {sims[0]:.3f}  p25 {sims[n//4]:.3f}  median {sims[n//2]:.3f}  p75 {sims[3*n//4]:.3f}  max {sims[-1]:.3f}")
    for thr in (0.6, 0.7, 0.8):
        print(f"  transcript chunks with best-match sim >= {thr}: {sum(1 for s in best_sim if s>=thr)}/{n}")

    # Monotonicity: among confidently-matched chunks (sim>=0.7), does the best
    # jowett position increase with transcript position? Longest non-decreasing
    # subsequence / count = fraction explained by a single monotonic mapping.
    conf = [(i, best_idx[i]) for i in range(n) if best_sim[i] >= 0.7]
    seq = [j for _, j in conf]
    # LNDS length
    import bisect
    tails = []
    for x in seq:
        k = bisect.bisect_right(tails, x)
        if k == len(tails): tails.append(x)
        else: tails[k] = x
    lnds = len(tails)
    print(f"\nconfident matches (sim>=0.7): {len(conf)}/{n}")
    if seq:
        print(f"longest non-decreasing run of jowett positions: {lnds}/{len(seq)} ({100*lnds/len(seq):.0f}%) -> monotonic correspondence")
    # Show a few example matches across the book
    print("\n--- sample matches (transcript chunk -> best jowett chunk) ---")
    for i in range(0, n, max(1, n//6)):
        print(f"[tr {i} -> jo {best_idx[i]} sim {best_sim[i]:.2f}]")
        print(f"   AUDIO : {tr_chunks[i][:140]}")
        print(f"   JOWETT: {jo_chunks[best_idx[i]][:140]}")

if __name__ == "__main__":
    main()
