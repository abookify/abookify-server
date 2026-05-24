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

### Approach 1 on a second slice — `02.mp3` (audio minutes 60–70)

Same protocol, different passage of the book. Slice covers the end of part6 (CIA — Chef Bernard / Escoffier Room) into the start of part7 (THE RETURN OF MAL CARNE — Dimitri intro). Prompt v2 (`prompt_v2_02.txt`) is tailored to EPUB parts 4–7: people (Mario, Dimitri, Bobby, Tyrone, Howard Mitcham, Chef Bernard, Hunter Thompson, Iggy Pop, …), places (Provincetown, Cape Cod, Vassar, Hyde Park, Culinary Institute of America, Khe Sanh, …), terms (Dreadnaught, Larousse Gastronomique, Escoffier Room, chaud-froid, demi-glace, sous-chef, garde-manger, Grill Bitch, Mal Carne, Tant pis).

#### Wins

| Audio passage | Baseline | Approach 1 | EPUB |
|---|---|---|---|
| pop-culture title | `night of the living dead` | `Night of the Living Dead` | Night of the Living Dead ✓ |
| sentence verb | "and **the most** withering" | "and **unload** the most" | "unload the most" ✓ |
| description | "shredding **the world**" | "shredding **bread**" | shredding bread ✓ |
| Dimitri biography | "fluent in Russian **literature**, master of English and German" | "fluent in Russian and German … amazing command of English" | "fluent in Russian and German as well as having an amazing command of English" ✓ |
| ending of preamble | "I had field experience, **a vocation**. I had **a good** vocabulary and a criminal mind" | "I had field experience, a vocabulary, and a criminal mind" | "a vocabulary and a criminal mind" ✓ |
| dittography | "outrage. **rage**, I submitted" (dropped word and merged) | "outrage. I submitted" | "outrage. I submitted" ✓ |
| paragraph recovery | "I'd seen a much-advocated, **highly-educated, and highly-educated, and highly-educated** chef on the stage, in the form of a" (garbled, no real content) | "I'd seen a much-admired commemorative cake, depicting Nixon painted in chalk, chocolate on a pastillage cameo, communicating by telephone with the Apollo astronauts in their space module, also chocolate on pastillage." | "much admired commemorative cake, depicting Nixon, painted in chocolate on a pastillage cameo, communicating by telephone with the Apollo astronauts in their space module, also chocolate on pastillage" — approach 1 recovered a passage baseline collapsed into nonsense |

The commemorative-cake recovery is the headline result. Baseline produced text a reader couldn't make sense of. Approach 1 produced text that, with one wrong word (`chalk` vs `chocolate`), matches the book. This is the kind of error initial_prompt clearly *does* fix: when baseline's beam search runs out of plausible-text options and starts repeating, the prompt's named entities (`commemorative cake`, `Apollo`, `pastillage`) give it traction.

#### Losses

| Audio passage | Baseline | Approach 1 | EPUB | Notes |
|---|---|---|---|---|
| French reference | `Larousse gastronomique` | **`LaRouche gastronomique`** | Larousse Gastronomique | Larousse *was in the prompt* — Whisper still mis-decoded it as the US conspiracy theorist's name. Striking failure: the prompt biased toward novel-fitting, then a familiar-sounding ASR output won anyway. |
| number | "thirty seconds into the chef's tirade" | "thirty-six. He did not let..." | "thirty seconds" ✓ | Approach 1 dropped half a sentence here |
| informal | "Bernard's **gonna**" | "Bernard's going to" | gonna ✓ | Approach 1 formalised informal speech |
| display cart | "voiture" | "Voiture" | voiture (lowercase) ✓ | Cosmetic, but prompt encouraged title-casing on French nouns |
| chapter heading | `E-Room` | `e-room` (lowercased) | "'E Room'" | Both wrong; baseline closer |

#### The Larousse → LaRouche failure

This deserves attention. The prompt explicitly contains "Larousse Gastronomique." Whisper still picked "LaRouche" — almost certainly because the audio realisation of `/læˈruːs/` sits closer in feature space to "LaRouche" for a US-tuned acoustic model. Putting the right word in the prompt biases the *language model* component, but doesn't override the *acoustic* mismatch when the right word is acoustically distant from the model's nearest English neighbour. This is exactly the failure mode where approach 3 (post-pass fuzz-correction against a vocabulary built from the EPUB) would win: edit-distance from "LaRouche" to "Larousse" is small enough that a constrained dictionary substitution catches it cleanly.

#### Same patterns as 01.mp3

- Prompted proper nouns mostly land (commemorative cake, Apollo, Night of the Living Dead, Bernard, Dimitri, Tyrone all correct in approach 1).
- Dittographies in baseline get cleaned up by approach 1 ("highly-educated, and highly-educated" → real text; "outrage. rage," → "outrage. I").
- Approach 1 sometimes formalises informal speech ("gonna" → "going to") or loses a few words to paraphrase.
- Audio-ambiguous words (baking class vs baking glass — both transcripts heard glass) are *not* fixed by either approach. Wisdom: initial_prompt only fixes language-model errors, not acoustic-frontend errors.

#### Score (this slice)

| Category | Count |
|---|---|
| Clear wins for approach 1 | 7 |
| Clear losses for approach 1 | 5 |
| Mixed / draws | ~6 |

Same shape as 01.mp3: net positive, same kinds of wins, same kinds of losses, plus a new failure mode (acoustic-distant prompt entry not honoured).

## Approach 2 — post-STT alignment to EPUB (not started)

After STT, align the resulting word sequence to EPUB prose using edit-distance / needle-in-haystack matching, and substitute the EPUB spelling where alignment is locally confident. This is exactly the "Phase 1 sync differentiator" pipeline that PROJECT_STATUS.md flags as the next big thing — so approach 2 isn't experimental orthogonal-work, it's the production path. The transcript-correction angle is a side-benefit of an alignment we want to build anyway.

What's already in place: `internal/library/text_align.go` does word-level alignment from Whisper timestamps to original text (designed for our own TTS round-trip though, where text is exact). Extending it to handle real-world STT noise needs the edit-distance step.

## Approach 3 — vocabulary fuzz-correction dictionary (not started)

Cheapest approach: a dictionary of `{whisper-output: correct-spelling}` pairs harvested from the EPUB. Apply as a post-pass to the transcript. Catches the *demi-glace / prep drone / Eric Ripert* class entirely without any alignment. Worst on words where the audio ambiguity is real (Cherbourg vs Cabourg), since there's no audio context to disambiguate.

Could be layered with approach 1 — use the prompt to bias Whisper, then a fuzz pass to clean up the leftover spelling errors.

## Files

All under `engineering/server/testdata/transcription-experiments/kitchen-confidential/`:

- Test slices: `runs/slice_01_0-600.mp3`, `runs/slice_02_0-600.mp3` (mp3 + epub-text are gitignored — derivable from the user's own files)
- Baseline transcripts: `runs/baseline_01_0-600.json`, `runs/baseline.txt`; `runs/baseline_02_0-600.json`, `runs/baseline_02.txt`
- Approach 1 transcripts: `runs/approach1_01_0-600.json`, `runs/approach1.txt`; `runs/approach1_02_0-600.json`, `runs/approach1_02.txt`
- EPUB-extracted prose: `epub-text/part*.txt`
- Prompts: `prompt_v1.txt` (parts 2-3 region, used on 01.mp3), `prompt_v2_02.txt` (parts 4-7 region, used on 02.mp3)

## Next

1. ~~Move the experiment artefacts out of `/tmp` into `testdata/`.~~ Done.
2. Once atrium GPU is back: rerun both slices on GPU to confirm the wins/losses reproduce (large-v3 + same settings should be deterministic, but worth a sanity check), then run a full chapter to widen the comparison.
3. Write the EPUB → prompt extractor (Go, under `internal/library/epub_prompt.go`) so prompts are automatic per chunk. Two slices is enough signal to design the harvester — multi-cap nouns + hyphenated compounds + non-ASCII tokens cover most wins; a small curated culinary lexicon supplements.
4. **Approach 3 (vocab fuzz-correction) moves up in priority.** The 02.mp3 results show prompted-but-not-honoured failures like Larousse → LaRouche. Fuzz-correction against an EPUB-derived vocabulary would catch exactly these. Should be tested on the same two slices before approach 2.
5. Start approach 2 on the same slices — measure incremental gain on top of approaches 1+3.

---

## Side discovery (2026-05-23): Forced alignment can't be field-tested chunk-by-chunk

While bootstrapping `whisper_transcript` peer books to enable forced alignment on Frankenstein, ran into a pipeline-shape mismatch worth recording.

**Observed wall-clock cost on local CPU:** chapter-015 (13 min audio) took 31 min on `large-v3 int8` — ~2.4× realtime, not the ~1.3× I'd been estimating from the Kitchen Confidential slices. Frankenstein full run (~7 h audio) projects to **~17 h wall**, not the ~10 h I was quoting. The KC slices were probably finishing inside warm-cache windows.

**Sidecar write failed (permission denied):** `testdata/library/abooks/Frankenstein.../audio/` is owned `root:root` from an older `docker compose up` run (the server container was writing into the bind mount as root). The host user (pj) can't write a sidecar there. Fixable with one docker chown.

**More fundamental: the pipeline expects whole-audiobook sidecars.**

`watcher.go::importSidecar` matches a sidecar to its work by:
1. Walking `works` and taking `wk.AudioFiles[0]` (the first audio file of the work).
2. Calling `findSidecar(af.Path)`, which checks (in order): `parent-dir.stt.json` (whole-audiobook), `<audioBase>.stt.json` (per-file), `<audioBase>.mp3.stt.json` (full-name).
3. Comparing the candidate path to the observed sidecar — must match exactly.
4. **Skipping the work if `sync_data` is already non-empty.**

For Frankenstein:
- A sidecar at `audio/chapter-015.stt.json` wouldn't match — only `audio/chapter-007.stt.json` (or whichever sorts first as AudioFiles[0]) would. So per-chapter sidecars don't reach `importOneSidecar`.
- Even a dir-level `audio.stt.json` would be skipped because Frankenstein already has `sync_data` from the early STT runs.

The escape hatch is `.stt.json.redo` — dropping that marker forces re-import even with existing sync_data. But it requires a completed `.stt.json` next to it.

`cmd/stt-cli/redo.go` explicitly documents: *"Sidecar must already exist at the output path. We don't fabricate a new sidecar from a partial run — call the normal mode first."* So `--redo-files` cannot bootstrap an incremental sidecar — it only patches an existing complete one.

**Three real paths forward:**

1. **Full Frankenstein STT in one ~17 h CPU run.** Produces a proper directory-mode sidecar that the watcher imports cleanly (plus an `.stt.json.redo` to force re-import over existing sync_data). Aligns with the existing pipeline. Slow.
2. **~50 lines of Go in `cmd/stt-cli/`: a `--bootstrap-sidecar` flag** that synthesises a minimal stub sidecar from the audio dir's file list (sources + zero-length words + duration), so `--redo-files` can subsequently fill it chapter-by-chapter. This makes "slow path, chunk by chunk" actually work, and the work persists across sessions.
3. **Different book.** None of the test books are both small and EPUB-paired. The Moral Landscape (4 mp3s = 6.8 h audio + EPUB) is similar size to Frankenstein. Project Hail Mary already has a transcript but no EPUB. No quick win here.

Path 2 is the right architectural fix if we want incremental transcription to be a first-class workflow. It also unblocks the same workflow on GPU later (interruptible across a power-cycle). Path 1 is the right move if we want field-tested alignment data this week.

### Resolution: path 2 implemented (2026-05-23)

`stt-cli --bootstrap-sidecar` (commit `15939f9`) writes a stub sidecar with the sources list + total duration but no words. `--redo-files <chapter>` then fills the stub one file at a time, merging into whatever's already there. Workflow:

```bash
stt-cli --audio ./audiobook-dir --bootstrap-sidecar
stt-cli --audio ./audiobook-dir --redo-files chapter-007.mp3   # ~40 min CPU per file
stt-cli --audio ./audiobook-dir --redo-files chapter-008.mp3   # etc.
touch ./audiobook-dir.stt.json.redo                            # force re-import
curl -X POST http://localhost:7654/api/works/{id}/align         # produce alignment
```

The watcher rejects empty-words sidecars on import, so the stub itself doesn't touch the DB. Re-import happens via the `.redo` marker after enough words have landed.

### First field-test result on Frankenstein

- chapter-015.mp3 (13 min audio) redo finished in 30 min wall (2.3× realtime on `large-v3 int8` CPU).
- Watcher imported the sidecar via the redo marker: created the `whisper_transcript` book (id 24795 in the local DB) with 2142 words attached.
- `POST /api/works/1/align` returned `{"chapters_aligned":1, "average_confidence":0.282}`. **First row ever in the `alignments` table.** 21 paragraph pairs serialised, ~1.5 KB.
- Semantic caveat: the alignment ran on chapter pair (0,0) — but the transcript for "chapter 0" of the transcript book is actually chapter-015 audio, because we only filled one file and chapter detection found no narrator markers in a single-file transcript. So the alignment is structurally valid but semantically mismatched. Will resolve as more chapters fill in and chapter-detect produces a real chapter sequence.
- Wall-time projection for the rest of Frankenstein's 9 testdata files: ~6 hours sequential. The full library (~7 h audio total in testdata) is the test-bed; full Frankenstein on disk has more files outside testdata.

The pipeline is end-to-end working. The next steps are content, not code: keep filling chunks, watch alignment confidence climb as the transcript gets denser and chapter-detect can sequence the audio.

### After 2/9 chunks (chapter-015 + chapter-007)

- Transcript book now has 3 chapters / 5011 words (chapter-detect found markers in the multi-file sidecar).
- EPUB book has 31 chapters / 78230 words.
- `chapter_links` table has 12 entries — most ebook chapters still unlinked.
- Re-align: `{"chapters_aligned":3, "average_confidence":0.166}`. Pairs JSON grew 7× (1577 → 10647 bytes).
- **Confidence DROPPED** (0.282 → 0.166) despite more data. Expected at low fill rates: with only 2/9 audio files, the linker can only pair up a handful of chapters, and the chapter-detect output for the transcript book doesn't yet correspond to the EPUB's chapter numbering. So most aligned pairs are between mismatched chapters — DP finds a "best" alignment between mismatched content, but the score is low. This will resolve as fill rate climbs and the linker has enough data to assemble a coherent narrative sequence.

This is a useful early signal that **`average_confidence` is not monotonic in fill rate**. At low fill rates it can drop because more partial-content chapters mean more low-quality alignments. The right metric to watch over the rest of this experiment is per-chapter-pair confidence on chapters where both sides are complete.
