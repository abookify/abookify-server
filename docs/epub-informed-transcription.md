# EPUB-informed transcription experiment

**Status:** Approach 1 (Whisper `initial_prompt`) baseline measured on Kitchen Confidential. Approaches 2 (post-STT alignment) and 3 (vocabulary fuzz-correction) not yet started.

## Setup

- **Test material:** *Kitchen Confidential* by Anthony Bourdain, author-read. Audiobook at `/mnt/raid/audiobooks/Anthony Bourdain - Kitchen Confidential Audiobook/` (9 mp3 files, ~60 min each, ~8 h total). EPUB on disk: Ecco hardcover edition, ISBN 9781582340821, calibre-built.
- **Test slice:** First 10 minutes of `01.mp3` (covers the audiobook intro + EPUB part2 *APPETIZER — A NOTE FROM THE CHEF* + start of part3 *FIRST COURSE — FOOD IS GOOD*).
- **Model:** faster-whisper `large-v3`, CPU `int8`, local Docker container. ~1.3× real-time (10-min slice = ~13 min decode).
- **Service change:** `services/whisper/server.py` now accepts an `initial_prompt` multipart form field; passes it to `model.transcribe(initial_prompt=…)`. Whisper truncates internally to its last-224-BPE-tokens window. (Commit `bbf7d77`.)

## Approach 1 — `initial_prompt` with EPUB-derived proper nouns

### Prompt used

```
Anthony Bourdain, Kitchen Confidential. Chefs and terms: Monsieur Saint-Jour,
Oncle Gustav, Tante Jeanne, Chuck Wepner the Bayonne Bleeder, Ali-Frazier,
Tintin, Astérix, Vélo-Solex, Pyramide, La Teste, Arcachon, Cherbourg, Normandy,
Vienne, Evian, Saint-Exupéry, Hotel Lutetia, Rover Sedan Mark III,
croque-monsieur, vin ordinaire, sandwich au jambon, demi-glace, grillardin,
saucier, sous-chef, garde-manger, mise en place, brigade de cuisine, prep drone,
fry cook, dishwasher, Les Halles, Park Avenue, Manhattan.
```

518 bytes. Built by hand from proper-nouns + culinary terms harvested out of EPUB parts 2 and 3 (the chapters the test slice covers). Not yet automated.

### Method

Both runs are exactly the same audio (`slice_01_0-600.mp3`) and same Whisper model + flags. The only difference is the `initial_prompt`. Each run takes ~13 minutes wall-clock on CPU `large-v3`.

Diff: `diff -u baseline.txt approach1.txt`. Ground truth: matched against EPUB parts 2-3 (file `text/part2.txt` + `text/part3.txt`).

### Results

#### Clear wins (approach 1 matched EPUB, baseline did not)

| Audio passage | Baseline | Approach 1 | EPUB ground truth |
|---|---|---|---|
| culinary term | `demiglass` | `demi-glace` | demi-glace ✓ |
| culinary role | `prep grown` | `prep drone` | prep drone ✓ |
| culinary role | `sous chef` (×3) | `sous-chef` (×3) | sous-chef ✓ |
| boxer nickname | `Bayonne bleeder` | `Bayonne Bleeder` | Bayonne Bleeder ✓ |
| chef name | `Eric Repair` | `Eric Ripert` | Eric Ripert ✓ |
| French interjection | `Tom P., man.` | `Tant pis, man.` | Tant pis ✓ |
| brand/comic | `Velo-Solex` | `Velo Solex` | Velo Solex ✓ |
| comic | `Astérix` | `Asterix` | Asterix ✓ |
| dittography | "fellow writer. I'm calling up a fellow writer." (duplicated) | duplication removed | (audio has it once) |

The Bayonne / Tant pis / Eric Ripert wins are the most important — these were near-hallucinations in the baseline, plausible-looking English that an unprompted reader would never know was wrong.

#### Regressions (baseline matched EPUB, approach 1 did not)

| Audio passage | Baseline | Approach 1 | EPUB ground truth |
|---|---|---|---|
| Normandy town | `Cabourg` | `Cavour` | Cherbourg ✓ (both wrong, baseline closer) |
| "long climb to ___" | `chefdom` | `the top of the world` | chefdom ✓ |
| ship vibrated | `vibrated terribly` | `vibrated terrorously` | terribly ✓ |
| movie theatre | `Queen's movie theatre` | `Queens movie theatre` | Queen's ✓ |
| dropped phrase | "I don't think I'll be getting any more popular" | (sentence collapsed) | (audio has it) |
| ending of preamble | "It's all here. The good, the bad, and the ugly. **The end.**" | "It's all here. The good, the bad, and the ugly." | "The end" is in audio |

`chefdom` and `vibrated terribly` are interesting — the prompt didn't bias toward fancier vocabulary on those, yet approach 1 chose more-elaborate alternatives. The prompt-style ("Chefs and terms: ___") may be priming Whisper toward "this is a literary text with foreign terms," which leaks into paraphrase territory.

`Cherbourg` is the most surprising loss — it *was* in the prompt — and yet Whisper still picked the wrong town in both runs. Suggests proper-nouns-in-prompt help with spelling but not necessarily with identification when the audio is ambiguous.

#### Mixed / draws

| Audio passage | Baseline | Approach 1 | EPUB ground truth |
|---|---|---|---|
| "Superchef" | `super chef` (lowercased) | `Super Chef` (two words) | Superchef ✓ (one word) |
| French dish | `moule marinier` | `Moulin Marinière` | moules marinieres ✓ (both wrong, different ways) |
| compound word | `worldview` | `world view` | world-view ✓ (hyphenated, neither matched) |
| compound word | `lawnmower` | `lawnmower` | lawn-mower (both lost hyphen) |

### Score

| Category | Count |
|---|---|
| Clear wins | 10 |
| Clear losses | 6 |
| Draws / cosmetic | ~5 |

Net positive, but approach 1 has a real failure mode: prompting nudges Whisper into looser paraphrasing on non-proper-noun spans, occasionally replacing common words with rarer near-synonyms ("chefdom" → "top of the world", "terribly" → "terrorously"). The fix isn't to abandon the approach — the wins on proper nouns are exactly what we want — but to layer something on top that *prefers the original EPUB wording* when an alignment is confident. That's approach 2.

### Generating the prompt automatically (proposed)

Hand-crafted prompts won't scale. A real implementation needs:

1. Identify which EPUB region the audio slice corresponds to (chapter or rough offset). For the experiment we cheated — we knew the first 10 min covered parts 2-3.
2. From that region of EPUB text, pull candidate terms:
   - Multi-word capitalised tokens (proper nouns)
   - Hyphenated compounds
   - Words with non-ASCII characters
   - Terms in a curated culinary lexicon (sous-chef, demi-glace, brigade de cuisine, …)
   - Optionally: low-frequency words (probably foreign or technical)
3. Cap to fit Whisper's 224-BPE window. Greedy include by frequency × distinctiveness.
4. Bind to the audio chunk on transcribe call.

The implementation needs to live in `cmd/stt-cli/` and `internal/stt/`, with the EPUB → prompt extractor probably under `internal/library/` since alignment lives there.

### Cost

- One 10-min slice = ~13 min wall on local CPU, ~30 s estimated on atrium GPU large-v3 (currently down).
- Iterating on different prompts at ~13 min/iter is tolerable for one passage but not for sweeping the book; need GPU before doing the full chapter.

## Approach 2 — post-STT alignment to EPUB (not started)

After STT, align the resulting word sequence to EPUB prose using edit-distance / needle-in-haystack matching, and substitute the EPUB spelling where alignment is locally confident. This is exactly the "Phase 1 sync differentiator" pipeline that PROJECT_STATUS.md flags as the next big thing — so approach 2 isn't experimental orthogonal-work, it's the production path. The transcript-correction angle is a side-benefit of an alignment we want to build anyway.

What's already in place: `internal/library/text_align.go` does word-level alignment from Whisper timestamps to original text (designed for our own TTS round-trip though, where text is exact). Extending it to handle real-world STT noise needs the edit-distance step.

## Approach 3 — vocabulary fuzz-correction dictionary (not started)

Cheapest approach: a dictionary of `{whisper-output: correct-spelling}` pairs harvested from the EPUB. Apply as a post-pass to the transcript. Catches the *demi-glace / prep drone / Eric Ripert* class entirely without any alignment. Worst on words where the audio ambiguity is real (Cherbourg vs Cabourg), since there's no audio context to disambiguate.

Could be layered with approach 1 — use the prompt to bias Whisper, then a fuzz pass to clean up the leftover spelling errors.

## Files

- Test slice: `/tmp/kc-experiment/runs/slice_01_0-600.mp3`
- Baseline transcript: `/tmp/kc-experiment/runs/baseline_01_0-600.json` and `.../baseline.txt`
- Approach 1 transcript: `/tmp/kc-experiment/runs/approach1_01_0-600.json` and `.../approach1.txt`
- EPUB-extracted prose: `/tmp/kc-experiment/text/part*.txt`
- Prompt used: `/tmp/kc-experiment/prompt_v1.txt`

These are under `/tmp` and will not survive a reboot. If we want to keep them, move under `engineering/server/testdata/transcription-experiments/`.

## Next

1. Move the experiment artefacts out of `/tmp` into `testdata/`.
2. Once atrium GPU is back: rerun both on GPU to confirm the wins/losses reproduce (large-v3 + same settings should be deterministic, but worth a sanity check).
3. Write the EPUB → prompt extractor (Go, under `internal/library/epub_prompt.go`) so prompts are automatic per chunk.
4. Start approach 2 on the same slice — measure incremental gain on top of approach 1.
