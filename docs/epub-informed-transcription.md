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

### GPU restored — full-book transcription + the real finding (2026-05-24)

atrium GPU came back (RTX 3060, 12 GB, driver 580.159). GPU whisper service (`device: cuda`, large-v3) runs at **~5× realtime** vs CPU's 0.4× — ~12× faster. Full Frankenstein (171 min audio across the 9 testdata files) transcribed in **34m40s** vs ~7 h projected on CPU. 28143 words, clean sidecar, pulled back and re-imported. Chapter-detect found 4 narrator chapters + 12 silence-based; linker connected 24/38 audio chapters to text.

Ran the alignment on the complete transcript. **Average confidence stayed at 0.16 — barely different from the partial CPU fills.** The per-chapter breakdown shows why, and it's the most important result of the experiment so far:

| ebook ch | conf | ebook words → transcript words | ratio |
|---|---|---|---|
| 0 | 0.312 | 170 → 2680 | 15.8× |
| 1 | 0.033 | 59 → 2242 | 38× |
| 2 | 0.252 | 1204 → 3777 | 3.1× |
| 3 | 0.244 | 1315 → 3937 | 3.0× |
| 5 | 0.099 | 2721 → 1561 | 0.57× |
| 6 | 0.079 | 1764 → 552 | 0.31× |
| 7 | 0.082 | 2210 → 642 | 0.29× |
| 9 | 0.161 | 2539 → 3251 | 1.28× |
| 10 | 0.169 | 2361 → 3213 | 1.36× |

**The alignment DP is not the weak link — the chapter linking is.** Word-count ratios swinging from 0.29× to 38× mean ebook chapters are being paired with the wrong audio chapters. Root cause: the Frankenstein **testdata audio is a partial subset** — 9 files / 28k transcript words against the full 38-chapter / 78k-word ebook (~36% coverage, a contiguous middle slice). Most ebook chapters have no corresponding audio, so the linker forces bad pairs and the DP then "aligns" mismatched content at low score.

**Implications:**
1. Forced-alignment quality is gated by chapter-link correctness. Garbage links → garbage alignment, regardless of transcript quality. Any alignment-quality evaluation must start from a work where audio and ebook cover the *same* content with correct links.
2. `average_confidence` over all linked pairs is a poor headline metric. A per-pair confidence + word-ratio sanity check (ratio far from ~1.0 ⇒ probable mis-link) is far more diagnostic. Worth surfacing the ratio in the divergence report.
3. The Frankenstein testdata is not a good alignment fixture. A fully-covered audiobook+ebook is needed. **Kitchen Confidential is now the natural choice** — full author-read audio (9 files, ~8 h) + full EPUB, and GPU makes the transcription ~1 h instead of ~10 h. That also closes the loop with the approach-1 experiment (same book), letting us test EPUB-informed transcription AND alignment on one fixture.

### Next
- Transcribe full Kitchen Confidential on GPU (~1 h), import, align. Expect ratios near ~1.0 since audio and EPUB cover the same content — the real test of alignment quality.
- Consider adding word-ratio to the divergence report as a mis-link detector.

### KC full-book result — the chapter-linking bottleneck is the real blocker (2026-05-24)

Transcribed the complete Kitchen Confidential on atrium GPU: **493 min (8.2 h) in 58 min wall (~8.5× realtime)**, 101k words. Imported cleanly (audio + `publisher_epub` + `whisper_transcript` all present). This is the ideal fixture — complete author-read audio and the matching EPUB, same content end to end. Aligned it. Result:

- EPUB: **30 chapters**, 96,829 words. Transcript: **4 chapters**, 99,522 words. `chapter_links`: **0**.
- The aligner index-matched the few overlapping indices and produced 3 junk pairs (word ratios 1547×, 161×, 0.00×). avg conf 0.42 is meaningless — driven by a 4-ebook-word "chapter 0".

**Why:** KC is a memoir. Bourdain's narration has *named* sections ("FOOD IS GOOD", "FOOD IS SEX", "INSIDE THE CIA") — not "Chapter One / Chapter Two." The narrator-pattern chapter detector keys on numeric "Chapter N" / "Part N" sequences, so it found almost nothing and lumped the 8-hour transcript into 4 giant blobs. The EPUB, meanwhile, has clean 30-section structure. Chapter-level linking can't bridge 4 ↔ 30.

**This is the headline conclusion of the whole experiment.** Across all three runs the failure was the same root cause, reached three different ways:

| Fixture | Audio/EPUB content | Why alignment failed |
|---|---|---|
| Frankenstein (testdata) | partial + scrambled audio (36% of book) | mismatched content — linker paired wrong chapters |
| Frankenstein (complete) | full, matched 99% by word count | front-matter offset + per-volume number resets → 10 transcript vs 31 ebook chapters → 0 links |
| Kitchen Confidential | full, matched, ideal | transcript under-chapterized (4 vs 30) → no links |

**Forced alignment is architecturally gated on chapter-level linking, and chapter links break whenever the two sides don't share chapter structure.** That is the *common* case, not the edge case: continuous-narration audiobooks, memoirs, anything without spoken "Chapter N" announcements. Transcript quality and audio coverage are not the bottleneck — the `chapter_links` dependency is.

**Recommended fix (design, not yet built):** decouple forced alignment from chapter links.
1. **Anchored / banded whole-book alignment.** Align the full transcript word-stream against the full EPUB word-stream and let the alignment *discover* where transcript content lands in the EPUB — chapter/paragraph boundaries then fall out of the alignment instead of being a precondition. The current Needleman-Wunsch DP can't do this directly: 100k × 97k = ~9.7 B cells, far past the 50 M guard. Needs a banded/anchored approach — seed with high-confidence n-gram anchors (rare proper nouns make great anchors — see approach 1), then DP only within a diagonal band between anchors. O(n·band) instead of O(n·m).
2. **EPUB-title-anchored chapter segmentation.** Cheaper interim step: we *have* the EPUB section titles. Search the transcript for each EPUB chapter's opening words/title to segment the transcript to match the EPUB's chapter count, then run the existing per-chapter DP. Turns "4 vs 30" into "30 vs 30." Doesn't need the banded aligner, reuses everything else.

Either path removes the chapter-link precondition. (2) is the smaller change and a natural next experiment; (1) is the robust long-term answer and also subsumes the divergence-detection and citation features.

### Complete Frankenstein result — confirmed, even with spoken chapter numbers (2026-05-24)

Swapped in the complete audiobook (`/mnt/raid/audiobooks/Shelley, Mary - Frankenstein`, 8 files / 8.6 h — the old testdata set was a broken partial slice). GPU transcription: **515 min in 58 min (~8.8× realtime)**, 77,583 words. Imported with all peers, EPUB origin fixed, aligned.

- EPUB **31 ch / 78,230 words**; transcript **10 ch / 77,465 words**; `chapter_links` **0**; index-fallback gave 8 garbage pairs (avg conf 0.19).
- **Word counts match to 99%** — the content corresponds almost perfectly. Same reading, same edition, end to end. Yet linking still failed, for reasons *different* from KC:
  1. **EPUB front-matter offsets every index** — EPUB 0 = Gutenberg boilerplate, 1 = CONTENTS, 2-5 = Letters 1-4, *then* Chapter 1. Audio has none of it.
  2. **Chapter numbers reset per volume** (3 volumes each restart at "Chapter 1"); the monotonic-sequence validator breaks at each reset and collapses whole volumes into blobs — transcript "Chapter 5" = 23,891 words, "Chapter 9" = 29,799 words. 10 detected vs 31 real.

So KC failed on named sections, Frankenstein failed on front-matter + volume resets — but both have content matching at the word level and both produced 0 links. **The transcripts are good; the chapter-correspondence requirement is the whole problem.**

### Verdict + build order

The 99%-matching word counts are the key: a banded whole-book aligner will work cleanly on both. Recommended order:
1. **Strip non-content EPUB front/back-matter** (Gutenberg header, CONTENTS, license) — cheap, helps every approach, removes the index offset.
2. **Banded/anchored whole-book alignment** (the real fix) — seed with rare proper-noun n-gram anchors (exactly the tokens approach 1 cared about), DP within a diagonal band. EPUB chapter/paragraph boundaries then project onto the audio timeline through the alignment — which is what karaoke + citations actually need.
3. EPUB-title-anchored per-chapter segmentation only where it applies; (2) subsumes it.

Needs human/product input on presenting partial + structurally-divergent matches — flagged with PJ to review the KC + Frankenstein data together.

### Anchor-density measurement (2026-05-25)

Before building the anchor aligner, measured how dense/unambiguous word-sequence anchors actually are on both imported works. Script: `testdata/transcription-experiments/anchor_density.py` (read-only against the DB). A "clean 1:1 anchor" = an n-gram that occurs exactly once in the ebook **and** exactly once in the transcript, after normalization (lowercase, strip punctuation), **exact match, no fuzzy**.

| n | KC clean anchors | KC gap med/p95/max | Frank clean anchors | Frank gap med/p95/max |
|---|---|---|---|---|
| 3 | 75,905 (76%) | 1 / 3 / 124 | 58,681 (75%) | 1 / 3 / 1558 |
| **4** | **82,082 (82%)** | **1 / 2 / 137** | **66,788 (85%)** | **1 / 1 / 2696** |
| 5 | 81,136 | 1 / 1 / 138 | 67,529 | 1 / 1 / 110 |
| 6 | 78,881 | 1 / 1 / 139 | 66,713 | 1 / 1 / 111 |

(percentages = clean anchors / ebook words; "gap" = words between consecutive clean anchors in ebook order)

Findings:
- **The texts are near word-identical in content.** ~80-85% of *every* 4-gram window in the ebook is a unique 1:1 anchor. Median anchor spacing is **1 word**. Anchors are not scarce — they're everywhere.
- **n=4 is the sweet spot**: most clean anchors, and tiny ambiguity (KC: ~428 "1-in-ebook-N-in-transcript", ~917 "appears-multiple-times-in-both"; Frank similar). The "out of the blue appears 10× in both" case is rare and resolved by monotonic ordering.
- **No frequency dictionary or rare-word selection needed.** At n=4 the density is so high we can discard every STT-risky or ambiguous candidate and still anchor every few words. Exact normalized matching already survives ~80%; the ~15-20% STT breaks are pure surplus.
- **Divergence falls out for free.** Frankenstein's 2,696-word max gap at n=4 is the **Project Gutenberg license** appended to the ebook (no audio). Confirmed by inspecting the gap text. This is the front/back-matter that fix #2 should strip — and a ready-made divergence test case.
- **Boilerplate cross-matches are a real hazard.** At n=5 a few license 5-grams spuriously matched the LibriVox closing announcement ("…in the public domain…") in the transcript — semantically wrong, textually identical. Reinforces stripping front/back-matter before alignment, and a robust divergence base case.

Conclusion: the recursive anchor aligner is not just viable, it's easy on this data. Building it next with synthetic known-answer tests.

### Anchor aligner built + validated (2026-05-25)

`internal/library/anchor_align.go` — pure-Go aligner: tokenize/normalize → hapax-in-ebook n-gram anchors that appear in the transcript → longest monotonic chain (LIS on transcript position) → classify gaps as aligned / ebook-only / trans-only / replace. No chapter correspondence required; divergence detection falls out of gap classification.

Tests:
- `anchor_align_test.go` — 10 synthetic known-answer cases (identical → full coverage; transcript intro → trans-only head; skipped paragraph → ebook-only; single STT error → bridged; out-of-order/repeated phrase → rejected by the chain; trailing Gutenberg license → ebook-only; ambiguous ebook n-gram → not anchored). CI-safe.
- `anchor_align_realdata_test.go` — runs against the local dev DB (skips in CI); aligns works 27/28 and reports coverage + largest divergences.

**Real-data result** (n=4, exact normalized matching, no fuzzy):

| Work | ebook words | transcript words | anchors | coverage | biggest divergence |
|---|---|---|---|---|---|
| Kitchen Confidential | 99,768 | 101,114 | 82,495 | **95.5%** | ~166-word section-transition gaps |
| Frankenstein | 78,604 | 77,597 | 67,161 | **96.4%** | ebook +3,012 / trans +49 at end = **Gutenberg license** |

Both books — which the chapter-link aligner could not align at all (0 usable links, garbage pairs) — reach **95-96% word-level coverage**, with the divergences landing exactly where expected: Frankenstein's trailing Gutenberg license (3,012 ebook words, ~no audio) and its front-matter header (237 words at position 0). KC's divergences are all small.

Note the end-of-Frankenstein divergence is classified `replace` rather than `ebook-only` because ~49 transcript words (the LibriVox public-domain outro) spuriously matched the license boilerplate — the front/back-matter cross-match again. **Fix #2 (strip non-content front/back-matter before alignment) would clean this up** and is the obvious next increment; the divergence is correctly *found* either way.

The chapter-correspondence requirement is gone, and the result is good on real audiobooks. Remaining work: (a) strip front/back-matter; (b) wire `Align` into the alignment pipeline as an alternative to / replacement for the chapter-link path, writing the anchor chain into the `alignments` table and projecting EPUB structure onto audio timestamps via the sidecar; (c) product decision on how divergences surface in the reader.

### Fixture sweep — coverage% is a one-number diagnostic across failure modes (2026-05-25)

Imported and aligned three more works (GPU-transcribed). The real-data test now auto-discovers every work with both peers. Results:

| Work | ebook words | transcript words | anchors | coverage | what coverage tells us |
|---|---|---|---|---|---|
| Kitchen Confidential | 99,768 | 101,114 | 82,495 | **95.5%** | same edition, complete |
| Frankenstein | 78,604 | 77,597 | 67,161 | **96.4%** | same edition, complete (trailing Gutenberg license = the gap) |
| Why We Sleep | 130,533 | 68,894 | 55,363 | **50.0%** | same edition, **partial audio** — biggest divergence is a single 63,636-word trailing ebook-only block; the audio cuts off mid-Chapter 8/9 ("…continues on the next disc", confirmed by ear). Science jargon did **not** hurt anchoring. |
| Plato's Republic vs all-dialogues EPUB | 1,209,154 | 45,379 | **641** | **0.2%** | **different translation.** See below. |

**New failure mode: translation/edition mismatch.** The Republic audiobook is a modern translation; the EPUB is Jowett's Victorian one. Opening line, audio: *"I went down to Piraeus yesterday with Glaucon, the son of Ariston, to offer a prayer to the goddess."* Jowett: *"I went down yesterday to the Piraeus with Glaucon the son of Ariston, that I might offer up my prayers to the goddess."* Same content, different words throughout → almost no 4-grams match → 641 lucky anchors (proper nouns + "I went down") → 0.2% coverage. **No lexical method (anchors or the chapter DP) can align across translations** — that needs semantic/embedding alignment, a much harder problem. For public-domain classics this is a real hazard: a free LibriVox audiobook often uses a *different* public-domain translation than your EPUB.

**Fifth fixture — Life of Pi (work 32): amateur/fan recording, 59.2% coverage.** ebook 104,834 words, transcript 117,019. The "audiobook" turns out to be a non-professional reading: the transcript tail is YouTube-style chatter ("…that's the end of chapter 26, thanks for joining me guys… I'll catch you in the next video, bye"), surfacing as a 28,105-word `trans-only` divergence; ~21k words of the book were skipped (a large `ebook-only` gap). Genuine narration still anchored (48,827 anchors). So coverage% + divergence content also distinguishes a **messy/amateur source** (chatter + gaps, two-way divergences) from a clean professional audiobook — and the `trans-only` spans literally contain the non-book commentary, which could be surfaced as "non-book content detected."

**The headline: `Coverage` cleanly separates all the cases, so it's the diagnostic to surface.**
- ~95% → same edition, complete (align + show)
- ~50% → same edition, partial audio (align the covered part, flag the rest as "no audio")
- ~0% → wrong pairing: different translation/edition, or simply not the same book. The system should **detect this and warn** ("these don't look like the same edition") rather than silently emit a broken alignment.

So beyond karaoke, coverage% is the signal that drives the right UX per work — and the diff-view minimap (PJ's idea) renders it directly: KC/Frankenstein nearly all green, Why We Sleep green-then-grey, Plato almost entirely grey with a few green flecks = "we can't line these up."

### Cross-translation alignment — strategy (2026-05-25)

The Plato case (same work, different translation → 0.2% lexical coverage) needs a non-lexical method. Two ideas, in increasing power:

**Why stopword-drop / content-word anchoring does NOT help (measured).** Republic audio vs *just* the Jowett Republic text (217k words):

| method | transcript n-grams found in Jowett-Republic |
|---|---|
| raw 4-gram | 5.3% |
| raw 3-gram | 17.3% |
| content-word 3-gram (stopwords dropped) | 2.1% |
| content-word 2-gram | 16.0% |

Dropping stopwords *hurt* consecutive matching — the content words also differ and reorder across translations. **But 92.4% of the audio's content-word tokens appear somewhere in the Jowett Republic** (3,049 / 3,790 distinct types shared). The vocabulary is nearly identical; only the order/phrasing differs. So the signal lives in *word sets*, not *word sequences*.

**Precursor (cheap): windowed set-overlap.** Slide a window over each side; match audio-paragraph ↔ text-paragraph by shared distinctive (rare, TF-IDF-weighted) content words, ignoring order. Buys coarse paragraph/section alignment. No model needed.

**Real fix: paragraph embeddings + DTW.** Embed each paragraph (the RAG pipeline already does this — `internal/llm/embed.go`, OpenAI `text-embedding-3-small` or Ollama). An embedding is a deterministic vector fingerprint of *meaning*, paraphrase-invariant by construction, so two translations of a paragraph land at near-identical vectors even with zero shared phrasing. Align the two vector sequences with Dynamic Time Warping (monotonic path maximizing cosine similarity) — the same shape as our anchor-chain LIS but scored by similarity instead of exact match. DTW absorbs 1↔2 paragraph splits and small local reorders. Gets **paragraph-level** correlation across translations (follow-along, cross-translation citations) — not word-level karaoke.

**Unified routing — coverage% picks the method:**
- ~95% anchor coverage → same edition → word-level anchor aligner (karaoke).
- ~0% anchor coverage → run embedding+DTW. It then distinguishes **different translation of the same work** (high paragraph-similarity → align at paragraph grain) from **a genuinely different book** (low similarity everywhere → correctly "these don't match").

**Test fixture is ready:** Plato Republic exists in two real translations on disk (audio transcript = modern; EPUB = Jowett). Prototype embedding+DTW on it to validate cross-translation alignment on real data.

### Embedding alignment PROVEN on Plato (2026-05-25)

Prototype `testdata/transcription-experiments/plato_embed_align.py` (runs on atrium, Ollama `nomic-embed-text`, no OpenAI). Chunked both texts into ~120-word windows (378 transcript / 1806 Jowett), embedded each, found each transcript chunk's best-cosine Jowett chunk.

Result — vs the **0.2%** lexical-anchor coverage on the same pair:

- best-match cosine: min 0.760, **median 0.847**, max 0.960.
- **All 378/378 transcript chunks matched a Jowett chunk at cosine ≥0.7** (360 at ≥0.8).
- **75% of matches fall in a single monotonic (non-decreasing) run** of Jowett positions — using *naive* best-match with no ordering constraint. DTW (which enforces monotonicity) would absorb most of the remaining 25% back-jumps.
- Matches are semantically correct across the translation gap: transcript chunk 0 ("I went down to Piraeus yesterday with Glaucon, the son of Ariston, to offer a prayer to the goddess") → Jowett "Book I I went down yesterday…"; the Ring of Gyges passage ("setting of the ring… became invisible") → Jowett "touching the ring he turned the collet outwards and reappeared"; the Form of the Good → Jowett's orb-of-light passage.
- Bonus: transcript chunk 0 matched Jowett chunk **822**, not 0 — because the Jowett "Republic" section opens with ~822 chunks of Jowett's *Introduction/Analysis* (not in the audio). The embedding match correctly skipped the scholarly front-matter and locked onto the dialogue's start. So embedding alignment handles front-matter divergence for free too.

### Anchor aligner wired into the pipeline (2026-05-25)

`ComputeAnchorAlignment(store, workID)` (`internal/library/anchor_pipeline.go`) is the production path: load ebook + transcript, **strip EPUB front/back-matter** (boilerplate chapters — Gutenberg headers/license, Contents, Index, Notes, colophon…), assemble content word streams with per-chapter global offsets, run the anchor aligner, and upsert an `alignments` row (`Method="anchor"`, `Unit="word"`, `Confidence`=coverage). `cmd/align-cli` runs it per-work or over all eligible works.

Backfilled all five fixtures (accurate coverage, after fixing an overlap over-count bug):

| Work | coverage | note |
|---|---|---|
| Frankenstein | 96.4% | complete, same edition |
| Kitchen Confidential | 92.9% | complete, same edition |
| Life of Pi | 57.0% | amateur recording (chatter + gaps) |
| Why We Sleep | 50.8% | only half the audio (missing discs) |
| Plato Republic | 0.1% | different translation (needs embeddings) |

Stored payload (`AnchorAlignmentPayload`, the `pairs` JSON for `anchor`-method rows) is self-contained for the reader: ebook + transcript `ChapterSpan`s (global-offset ↔ chapter/word), `Segment`s (aligned / ebook-only / trans-only / replace, global offsets), coverage, and a divergence summary (per-kind counts + biggest divergent spans). `MapEbookToTrans` maps an ebook range to the transcript range; compose with `TransChapters` + `sync_data` for audio time. Contract documented in `anchor_pipeline.go`; cross-session consumer notes in `engineering/SESSION_HANDOFF.md`.

**Conclusion: the cross-translation problem is solved at paragraph level.** Lexical anchoring (0.2%) → semantic embeddings (100% of chunks matched, 75%+ monotonic). The local `nomic-embed-text` model is more than adequate — no paid API needed. Productionizing = embed paragraphs (RAG pipeline already does this) + DTW over the cosine matrix + the same coverage/divergence reporting. Routing stays coverage-driven: high lexical coverage → word-level anchor karaoke; near-zero → embedding+DTW for paragraph-level cross-translation correlation.
