# TTS sameness: gpu-engine-baseline vs cpu-fastapi

chunks: 10 · voice: bm_fable · gate -45 dBFS · gap ≥ 150 ms

| metric | ref | cand | Δ (cand − ref) |
|---|---|---|---|
| assembled total (with pauses) | 216.206 s | 209.425 s | -6.781 s |
| Σ chunk duration | 211.706 s | 204.925 s | -6.781 s |
| Σ speech (dur − lead − trail) | 206.880 s | 194.730 s | -12.150 s |
| Σ leading silence | 0.610 s | 3.000 s | +2.390 s |
| Σ trailing silence | 4.216 s | 7.195 s | +2.979 s |
| internal gaps (count) | 79 | 77 | -2 |
| internal gaps (Σ secs) | 21.400 s | 21.470 s | +0.070 s |
| per-chunk Δ duration | | | mean -0.678 · max |Δ| 2.833 · p95 |Δ| 2.443 |
| per-chunk Δ speech | | | mean -1.215 · max |Δ| 3.580 |
| per-chunk Δ lead / trail (mean) | | | +0.239 / +0.298 |
| wall time | 150.9 s | 10.4 s | 14.4× faster |

Worst chunks by |Δ duration|:

| seg | words | ref | cand | Δ | Δ speech | Δ lead | Δ trail | gaps ref/cand | text |
|---|---|---|---|---|---|---|---|---|---|
| 6 | 117 | 38.058 | 35.225 | -2.833 | -3.580 | +0.280 | +0.467 | 14/13 | Oh! But he was a tight-fisted hand at the grindstone, Scroog |
| 7 | 79 | 24.490 | 22.525 | -1.965 | -2.290 | +0.290 | +0.035 | 11/8 | External heat and cold had little influence on Scrooge. No w |
| 2 | 79 | 22.154 | 20.675 | -1.479 | -2.170 | +0.270 | +0.421 | 7/8 | Mind! I don’t mean to say that I know, of my own knowledge,  |
| 3 | 83 | 23.451 | 22.200 | -1.251 | -1.920 | +0.250 | +0.419 | 8/9 | Scrooge knew he was dead? Of course he did. How could it be  |
| 1 | 58 | 17.192 | 15.975 | -1.217 | -1.660 | +0.230 | +0.213 | 4/6 | Marley was dead: to begin with. There is no doubt whatever a |
| 4 | 104 | 29.579 | 30.725 | +1.146 | +0.550 | +0.210 | +0.386 | 13/11 | The mention of Marley’s funeral brings me back to the point  |
| 8 | 109 | 27.737 | 28.650 | +0.913 | +0.180 | +0.240 | +0.493 | 11/10 | Nobody ever stopped him in the street to say, with gladsome  |
| 5 | 52 | 16.309 | 15.825 | -0.484 | -1.250 | +0.230 | +0.536 | 6/6 | Scrooge never painted out Old Marley’s name. There it stood, |
