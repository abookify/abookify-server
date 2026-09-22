# TTS sameness: gpu-engine-ported vs cpu-fastapi

chunks: 177 · voice: bm_fable · gate -45 dBFS · gap ≥ 150 ms

| metric | ref | cand | Δ (cand − ref) |
|---|---|---|---|
| assembled total (with pauses) | 2037.417 s | 2036.445 s | -0.972 s |
| Σ chunk duration | 1949.417 s | 1948.445 s | -0.972 s |
| Σ speech (dur − lead − trail) | 1872.010 s | 1872.770 s | +0.760 s |
| Σ leading silence | 11.920 s | 11.930 s | +0.010 s |
| Σ trailing silence | 65.487 s | 63.745 s | -1.742 s |
| internal gaps (count) | 561 | 571 | +10 |
| internal gaps (Σ secs) | 150.310 s | 151.920 s | +1.610 s |
| per-chunk Δ duration | | | mean -0.005 · max |Δ| 0.431 · p95 |Δ| 0.176 |
| per-chunk Δ speech | | | mean +0.004 · max |Δ| 0.490 |
| per-chunk Δ lead / trail (mean) | | | +0.000 / -0.010 |
| wall time | 902.2 s | 47.8 s | 18.9× faster |

Worst chunks by |Δ duration|:

| seg | words | ref | cand | Δ | Δ speech | Δ lead | Δ trail | gaps ref/cand | text |
|---|---|---|---|---|---|---|---|---|---|
| 148 | 91 | 28.929 | 28.499 | -0.431 | -0.460 | +0.000 | +0.029 | 9/8 | “Oh! captive, bound, and double-ironed,” cried the phantom,  |
| 119 | 76 | 22.758 | 22.328 | -0.430 | -0.220 | +0.000 | -0.210 | 8/8 | To sit, staring at those fixed glazed eyes, in silence for a |
| 88 | 81 | 26.548 | 26.190 | -0.358 | +0.000 | +0.000 | -0.358 | 9/9 | Sitting-room, bedroom, lumber-room. All as they should be. N |
| 20 | 112 | 31.946 | 31.675 | -0.271 | -0.240 | +0.000 | -0.031 | 11/11 | “What else can I be,” returned the uncle, “when I live in su |
| 2 | 79 | 21.897 | 22.156 | +0.258 | +0.260 | +0.000 | -0.002 | 7/6 | Mind! I don’t mean to say that I know, of my own knowledge,  |
| 170 | 31 | 8.999 | 9.242 | +0.243 | +0.000 | +0.000 | +0.243 | 2/2 | It beckoned Scrooge to approach, which he did. When they wer |
| 25 | 161 | 43.042 | 43.278 | +0.236 | +0.110 | +0.000 | +0.126 | 15/14 | “There are many things from which I might have derived good, |
| 133 | 45 | 10.496 | 10.268 | -0.228 | +0.000 | +0.000 | -0.228 | 4/5 | “I wear the chain I forged in life,” replied the Ghost. “I m |
