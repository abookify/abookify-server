# TTS sameness: cpu-run2 vs cpu-run1

chunks: 10 · voice: bm_fable · gate -45 dBFS · gap ≥ 150 ms

| metric | ref | cand | Δ (cand − ref) |
|---|---|---|---|
| assembled total (with pauses) | 215.757 s | 215.865 s | +0.108 s |
| Σ chunk duration | 211.257 s | 211.365 s | +0.108 s |
| Σ speech (dur − lead − trail) | 205.940 s | 206.100 s | +0.160 s |
| Σ leading silence | 0.610 s | 0.640 s | +0.030 s |
| Σ trailing silence | 4.707 s | 4.625 s | -0.082 s |
| internal gaps (count) | 75 | 72 | -3 |
| internal gaps (Σ secs) | 20.100 s | 19.290 s | -0.810 s |
| per-chunk Δ duration | | | mean +0.011 · max |Δ| 0.135 · p95 |Δ| 0.100 |
| per-chunk Δ speech | | | mean +0.016 · max |Δ| 0.110 |
| per-chunk Δ lead / trail (mean) | | | +0.003 / -0.008 |
| wall time | 107.3 s | 122.8 s | 0.9× faster |

Worst chunks by |Δ duration|:

| seg | words | ref | cand | Δ | Δ speech | Δ lead | Δ trail | gaps ref/cand | text |
|---|---|---|---|---|---|---|---|---|---|
| 6 | 117 | 37.771 | 37.905 | +0.135 | +0.110 | +0.030 | -0.005 | 15/12 | Oh! But he was a tight-fisted hand at the grindstone, Scroog |
| 9 | 39 | 11.160 | 11.102 | -0.058 | +0.000 | +0.000 | -0.058 | 5/5 | But what did Scrooge care! It was the very thing he liked. T |
| 1 | 58 | 17.152 | 17.192 | +0.040 | +0.040 | +0.000 | -0.000 | 3/3 | Marley was dead: to begin with. There is no doubt whatever a |
| 2 | 79 | 22.046 | 22.034 | -0.012 | -0.010 | +0.000 | -0.002 | 7/6 | Mind! I don’t mean to say that I know, of my own knowledge,  |
| 8 | 109 | 27.729 | 27.736 | +0.007 | +0.010 | +0.000 | -0.003 | 10/10 | Nobody ever stopped him in the street to say, with gladsome  |
| 7 | 79 | 24.491 | 24.487 | -0.005 | -0.000 | +0.000 | -0.005 | 10/11 | External heat and cold had little influence on Scrooge. No w |
| 5 | 52 | 16.307 | 16.309 | +0.002 | +0.000 | +0.000 | +0.002 | 6/6 | Scrooge never painted out Old Marley’s name. There it stood, |
| 4 | 104 | 29.580 | 29.579 | -0.001 | +0.000 | +0.000 | -0.001 | 11/11 | The mention of Marley’s funeral brings me back to the point  |
