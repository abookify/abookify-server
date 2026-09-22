# TTS sameness: gpu-engine-ported vs cpu-fastapi

chunks: 10 · voice: bm_fable · gate -45 dBFS · gap ≥ 150 ms

| metric | ref | cand | Δ (cand − ref) |
|---|---|---|---|
| assembled total (with pauses) | 216.104 s | 215.651 s | -0.453 s |
| Σ chunk duration | 211.604 s | 211.151 s | -0.453 s |
| Σ speech (dur − lead − trail) | 206.260 s | 206.390 s | +0.130 s |
| Σ leading silence | 0.610 s | 0.640 s | +0.030 s |
| Σ trailing silence | 4.734 s | 4.121 s | -0.613 s |
| internal gaps (count) | 73 | 77 | +4 |
| internal gaps (Σ secs) | 20.180 s | 20.330 s | +0.150 s |
| per-chunk Δ duration | | | mean -0.045 · max |Δ| 0.354 · p95 |Δ| 0.249 |
| per-chunk Δ speech | | | mean +0.013 · max |Δ| 0.420 |
| per-chunk Δ lead / trail (mean) | | | +0.003 / -0.061 |
| wall time | 85.0 s | 8.8 s | 9.6× faster |

Worst chunks by |Δ duration|:

| seg | words | ref | cand | Δ | Δ speech | Δ lead | Δ trail | gaps ref/cand | text |
|---|---|---|---|---|---|---|---|---|---|
| 2 | 79 | 22.261 | 21.908 | -0.354 | -0.210 | +0.000 | -0.144 | 7/6 | Mind! I don’t mean to say that I know, of my own knowledge,  |
| 6 | 117 | 37.925 | 37.803 | -0.122 | -0.150 | +0.030 | -0.002 | 12/15 | Oh! But he was a tight-fisted hand at the grindstone, Scroog |
| 5 | 52 | 16.279 | 16.307 | +0.028 | +0.030 | +0.000 | -0.002 | 6/6 | Scrooge never painted out Old Marley’s name. There it stood, |
| 7 | 79 | 24.491 | 24.483 | -0.007 | -0.010 | +0.000 | +0.003 | 11/11 | External heat and cold had little influence on Scrooge. No w |
| 9 | 39 | 11.160 | 11.158 | -0.002 | +0.000 | +0.000 | -0.002 | 5/5 | But what did Scrooge care! It was the very thing he liked. T |
| 4 | 104 | 29.578 | 29.579 | +0.001 | +0.000 | +0.000 | +0.001 | 11/11 | The mention of Marley’s funeral brings me back to the point  |
| 0 | 2 | 1.569 | 1.570 | +0.001 | +0.040 | +0.000 | -0.039 | 0/0 | Marley’S Ghost. |
| 1 | 58 | 17.191 | 17.192 | +0.001 | +0.010 | +0.000 | -0.009 | 4/3 | Marley was dead: to begin with. There is no doubt whatever a |
