# TTS sameness: gpu-run2 vs gpu-run1

chunks: 10 · voice: bm_fable · gate -45 dBFS · gap ≥ 150 ms

| metric | ref | cand | Δ (cand − ref) |
|---|---|---|---|
| assembled total (with pauses) | 215.888 s | 215.673 s | -0.214 s |
| Σ chunk duration | 211.388 s | 211.173 s | -0.214 s |
| Σ speech (dur − lead − trail) | 206.510 s | 206.960 s | +0.450 s |
| Σ leading silence | 0.640 s | 0.610 s | -0.030 s |
| Σ trailing silence | 4.238 s | 3.603 s | -0.634 s |
| internal gaps (count) | 79 | 80 | +1 |
| internal gaps (Σ secs) | 20.880 s | 21.460 s | +0.580 s |
| per-chunk Δ duration | | | mean -0.021 · max |Δ| 0.310 · p95 |Δ| 0.308 |
| per-chunk Δ speech | | | mean +0.045 · max |Δ| 0.310 |
| per-chunk Δ lead / trail (mean) | | | -0.003 / -0.063 |
| wall time | 5.0 s | 4.9 s | 1.0× faster |

Worst chunks by |Δ duration|:

| seg | words | ref | cand | Δ | Δ speech | Δ lead | Δ trail | gaps ref/cand | text |
|---|---|---|---|---|---|---|---|---|---|
| 8 | 109 | 27.699 | 28.009 | +0.310 | +0.310 | +0.000 | +0.000 | 12/11 | Nobody ever stopped him in the street to say, with gladsome  |
| 9 | 39 | 11.101 | 10.796 | -0.305 | +0.000 | +0.000 | -0.305 | 6/5 | But what did Scrooge care! It was the very thing he liked. T |
| 7 | 79 | 24.487 | 24.191 | -0.296 | +0.000 | +0.000 | -0.296 | 10/11 | External heat and cold had little influence on Scrooge. No w |
| 1 | 58 | 17.153 | 17.290 | +0.137 | +0.150 | +0.000 | -0.013 | 4/5 | Marley was dead: to begin with. There is no doubt whatever a |
| 6 | 117 | 37.858 | 37.748 | -0.110 | -0.080 | -0.030 | -0.000 | 15/15 | Oh! But he was a tight-fisted hand at the grindstone, Scroog |
| 2 | 79 | 22.199 | 22.262 | +0.062 | +0.030 | +0.000 | +0.032 | 7/7 | Mind! I don’t mean to say that I know, of my own knowledge,  |
| 5 | 52 | 16.306 | 16.278 | -0.028 | -0.020 | +0.000 | -0.008 | 6/6 | Scrooge never painted out Old Marley’s name. There it stood, |
| 4 | 104 | 29.562 | 29.578 | +0.015 | +0.020 | +0.000 | -0.005 | 11/12 | The mention of Marley’s funeral brings me back to the point  |
