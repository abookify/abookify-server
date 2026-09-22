# Board 17 — GPU TTS sameness measurements (2026-09-21, tank RTX 3060)

Reference = Docker `kokoro-fastapi-cpu` 0.7.2 on :8880. Candidate = hermetic
engine `tts_server.py` on :8881 (CUDA). Same kokoro/misaki 0.9.4, same model +
voice bytes (md5), voice bm_fable, text = Carol Stave One as the generator
chunks it (`stave-one-chunks.json` from `TestDumpTTSChunks`).

| file | pair | Δ total | note |
|---|---|---|---|
| baseline-10 | raw engine vs CPU | −6.78 s / 216 s | the August defect reproduced: +5.4 s boundary silence, −12.2 s speech |
| ported-10 | ported engine vs CPU | −0.45 s / 216 s | 7/10 chunks within ±1 ms |
| control-cpu-10 | CPU vs CPU | +0.11 s | Kokoro's own run-to-run band on CPU |
| control-gpu-10 | GPU vs GPU | −0.21 s | …and on GPU |
| ported-full-stave-one | ported engine vs CPU, all 177 segments | −0.97 s / 2037 s | mean per-chunk −5 ms, max 0.43 s; GPU 47.8 s vs CPU 902 s (18.9×) |

Reproduce: `engine/tools/tts_sameness.py --chunks stave-one-chunks.json --ref … --cand … --out …`
(assembled WAVs are written to --out; not archived here).
