#!/usr/bin/env node
// Web journey runner (playwright-core + system chromium). Usage:
//   node run-web.js --base http://localhost:8199 --work 1 [--cookie TOKEN] [--shots DIR]
// Exit 0 = every journey passed; nonzero = loud failure with per-assert detail
// and a screenshot per failure in --shots.
//
// karaoke_advances is the assertion that matters (A1-A5). Its bar: it must
// FAIL on the five states we actually shipped broken — frozen highlight,
// wrong chapter shown, near-empty render, wrong sentence, frozen live-clock.
// It was calibrated against real broken data (main-server work 85) before
// being trusted; see testing/e2e/README-calibration in the handoff.
const fs = require('fs');
const args = {};
for (let i = 2; i < process.argv.length; i += 2) args[process.argv[i].replace(/^--/, '')] = process.argv[i + 1];
const BASE = args.base || 'http://localhost:8199';
const WORK = args.work;
const SHOTS = args.shots || '/tmp/e2e-shots';
const PW = args.pw || '/home/pj/projects/web/youarehereart/laravel-web/frontend/node_modules/playwright-core';
fs.mkdirSync(SHOTS, { recursive: true });
const { chromium } = require(PW);

const results = [];
// ---- RULE 4 harness (testing/RUNNER-CONTRACT.md): the harness absorbs a FIXED,
// NAMED set of known transients as bounded, RECORDED wait-loops; the runner never
// retries an action and never swallows an error. Every wait's attempt count and
// every drive error is an artifact printed in the report ("HARNESS WAITS" /
// "DRIVE ERRORS" blocks in finish()).
const waits = [];        // {label, outcome:'ready'|'TIMEOUT', attempts, ms}
const driveErrors = [];  // {journey, step, error} — errors a drive step raised; never silently dropped
function record(label, outcome, attempts, ms) { waits.push({ label, outcome, attempts, ms }); }
// Waits on a CONDITION becoming truthy — it never re-runs an action. Returns the
// satisfied value. On timeout: records TIMEOUT and, by default, THROWS (a wait a
// journey cannot proceed without). `soft:true` returns false instead so a journey
// that owns its own verdict (resume_flow's stuck/harness signatures) reads the
// honest state — the timeout is still in the artifacts either way.
async function waitFor(label, cond, { timeoutMs = 15000, intervalMs = 250, soft = false } = {}) {
  const t0 = Date.now();
  let attempts = 0;
  while (Date.now() - t0 < timeoutMs) {
    attempts++;
    try { const v = await cond(); if (v) { record(label, 'ready', attempts, Date.now() - t0); return v; } } catch {}
    await new Promise(r => setTimeout(r, intervalMs));
  }
  record(label, 'TIMEOUT', attempts, Date.now() - t0);
  if (soft) return false;
  throw new Error(`waitFor(${label}) timed out after ${attempts} attempts / ${Date.now() - t0}ms`);
}
// A drive step's error is EVIDENCE, not noise: record it under its journey. The
// journey's report() then carries it and fails — a drive that errored cannot
// produce a trustworthy green for the thing it was driving.
function driveErr(journey, step) { return (e) => { driveErrors.push({ journey, step, error: String(e && e.message || e).slice(0, 160) }); return null; }; }
function drivesClean(journey) { return !driveErrors.some(d => d.journey === journey); }
// What THIS run actually looked at — printed in the limits block so a green run
// is green about something specific, not "the whole book is fine".
const coverage = { karaokeChapter: null, karaokeWords: 0, karaokeAudioSec: null, sources: [] };
function report(id, ok, detail) {
  const de = driveErrors.filter(d => d.journey === id);
  if (de.length) { ok = false; detail = (detail || '') + ` DRIVE-ERROR ${de.map(d => d.step + ': ' + d.error).join(' | ')}`; }
  results.push({ id, ok, detail });
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${id}  ${detail || ''}`);
}
let uiClockRaw = async () => null; // bound once `page` exists (below)
function clockSecs(txt) { // "1:34" or "1:02:03" -> seconds
  const p = (txt || '').trim().split(':').map(Number);
  if (p.some(isNaN) || !p.length) return null;
  return p.reduce((a, b) => a * 60 + b, 0);
}

(async () => {
  const browser = await chromium.launch({ executablePath: '/usr/bin/chromium', args: ['--no-sandbox', '--autoplay-policy=no-user-gesture-required'] });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  if (args.cookie) await ctx.addCookies([{ name: 'abookify_session', value: args.cookie, domain: new URL(BASE).hostname, path: '/' }]);
  const page = await ctx.newPage();
  uiClockRaw = async () => clockSecs(await page.locator('.player-time').first().textContent().catch(() => null));
  const consoleErrors = [];
  page.on('pageerror', e => consoleErrors.push(String(e)));

  // ---- open_library
  // RULE 4 (testing/RUNNER-CONTRACT.md): 'networkidle' is a blind readiness wait
  // that NEVER fires against a busy live server (the WS + job-poll + cover loads
  // keep the network alive), so it FATAL'd the whole run under the showcase
  // queue. Absorb the real transient instead — wait for the library to actually
  // load its works — as a bounded, recorded loop.
  const libraryLoaded = () => page.evaluate(() =>
    typeof allWorks !== 'undefined' && Array.isArray(allWorks) && allWorks.length > 0).catch(() => false);
  await page.goto(BASE, { waitUntil: 'domcontentloaded' });
  const libOk = await waitFor('library-works-loaded', libraryLoaded, { timeoutMs: 30000, intervalMs: 300, soft: true });
  // `.work-card` ONLY — `[class*=card]` matched any class containing "card" (the
  // tripwire class: a guard looser than its intent can pass on the wrong thing).
  const cards = await page.locator('.work-card').count();
  report('open_library', libOk && cards >= 1 && consoleErrors.length === 0,
    `cards=${cards} pageErrors=${consoleErrors.length} works-loaded=${libOk}`);

  // ---- open_book (DETERMINISTIC: open the work under test by id, via the app's
  // own openWorkDetail, so the journey exercises a KNOWN work — not whichever
  // card happens to be first. server-web owns this navigation.)
  let opened = await page.evaluate((wid) => {
    if (typeof openWorkDetail !== 'function') return false;
    openWorkDetail(Number(wid));
    return true;
  }, WORK).catch(driveErr('open_book', 'openWorkDetail'));
  // Readiness = the detail's DATA landed, not its markup: #work-detail gets
  // data-hydrated=<id> only when every async hydrate step (chapter lists, resume
  // decoration) has settled. The first conversion waited on the per-work menu
  // button (#menu-btn-d-<id>), which exists ~5ms after openWorkDetail — 1.8s EARLIER
  // than the sleep it replaced — so the seeks/resume raced the in-flight hydrate and
  // landed at 0:00 (9/11 vs the old runner's 11/11 on the same server, same minute:
  // the old runner kept as a control is what caught it). A readiness condition
  // must mean what the sleep was standing in for, not merely fire sooner. Also not
  // `text=/AUDIOBOOK|EBOOK/i`, which matches the library's own copy.
  const detailRendered = () => page.locator(`#work-detail[data-hydrated="${WORK}"]`).count().then(n => n > 0);
  opened = !!opened && await waitFor('work-detail-rendered', detailRendered, { timeoutMs: 15000, soft: true });
  report('open_book', opened, opened ? `#work-detail hydrated for work ${WORK}` : 'openWorkDetail did not hydrate the work detail');
  if (!opened) { await page.screenshot({ path: `${SHOTS}/open_book-FAIL.png` }); process.exit(finish()); }

  // ---- SHAPE GATE (blind-spot register rows, closed 2026-08-15): text-only
  // and audio-only are real shipping shapes every journey below assumes away
  // (they all start with play). Detect the shape and run the dedicated
  // asserts instead; the standard journeys report n/a-by-shape rather than
  // failing on a work that legitimately has no audio or no text.
  const shape = await page.evaluate((wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    return { audio: (w.audio_files || []).length, text: (w.text_files || []).length };
  }, WORK);
  // Degraded-testimony board (task 12 / blind-spot register): a work whose
  // canon carries degraded production testimony certifies the DATA contract
  // here (reason must be present); the UI pill assert joins when server-web's
  // selector exists. Detected via canon so the assert reads the same contract
  // every surface does.
  const canonPeek = await page.evaluate(async (wid) => {
    const r = await fetch(`/api/works/${wid}/canon`);
    return r.ok ? r.json() : null;
  }, WORK);
  const degraded = canonPeek && [...(canonPeek.texts || []), ...(canonPeek.editions || [])]
    .find(x => x.condition === 'degraded');
  if (degraded) {
    // Report the testimony and CONTINUE — degraded is a caveat, not
    // unusable (a live work with a flagged transcript still plays, reads
    // and navigates; scoping it out would hide exactly the experience the
    // caveat annotates). The 8192 fixture board runs the same way.
    report('degraded_testimony', !!(degraded.condition_reason || '').length,
      `condition=degraded reason="${(degraded.condition_reason || 'MISSING').slice(0, 60)}" — testimony present and reasoned`);
  }
  if (shape.audio === 0 || shape.text === 0) {
    const kind = shape.audio === 0 ? 'TEXT-ONLY' : 'AUDIO-ONLY';
    if (shape.audio === 0) {
      // Reader-only experience: chapters render, navigation works, no
      // player assumed anywhere.
      const r = await page.evaluate(async (wid) => {
        const w = (allWorks || []).find(x => x.id === Number(wid));
        const tf = (typeof displayEditionBooks === 'function') ? displayEditionBooks(w, 'text')[0] : w.text_files[0];
        await loadChapterList(tf.id, w.id);
        const chs = (chapterCache[tf.id]?.chapters) || [];
        if (!chs.length) return { ok: false, why: 'no chapters' };
        // In-page bounded polls (the harness primitive, inside evaluate): wait on the
        // reader's CHAPTER STATE, not a fixed sleep; attempts come back in the result.
        const until = async (cond, max = 40) => { for (let i = 1; i <= max; i++) { if (cond()) return i; await new Promise(res => setTimeout(res, 250)); } return -max; };
        await loadChapter(tf.id, chs[0].index, w.id);
        const a1 = await until(() => currentReaderChapter[w.id] && currentReaderChapter[w.id].index === chs[0].index && document.body.innerText.length > 500);
        const len1 = document.body.innerText.length;
        const target = chs.length > 1 ? chs[1].index : chs[0].index;
        await loadChapter(tf.id, target, w.id);
        const a2 = await until(() => currentReaderChapter[w.id] && currentReaderChapter[w.id].index === target);
        const cur = currentReaderChapter[w.id];
        return { ok: len1 > 500 && cur && cur.index === target, chapters: chs.length, rendered: len1, waits: { 'reader-chapter-0': a1, 'reader-chapter-target': a2 } };
      }, WORK).catch(e => ({ ok: false, why: String(e).slice(0, 80) }));
      for (const [k, v] of Object.entries(r.waits || {})) record(k, v > 0 ? 'ready' : 'TIMEOUT', Math.abs(v), Math.abs(v) * 250);
      report('reader_only', !!r.ok, r.ok ? `chapters=${r.chapters} rendered=${r.rendered} nav ok, no player` : (r.why || 'reader did not render/navigate'));
    } else {
      // Pre-transcription experience: audio plays, the Transcribe CTA is
      // offered, nothing crashes for lack of a reader.
      const r = await page.evaluate(async (wid) => {
        const w = (allWorks || []).find(x => x.id === Number(wid));
        const a = w.audio_files[0];
        playAudio(a.id, 0, w.id, a.title || a.filename, w.title);
        const el = document.getElementById('audio-player');
        let attempts = 0;
        for (attempts = 1; attempts <= 40; attempts++) { if (el && !el.paused && el.currentTime > 1) break; await new Promise(res => setTimeout(res, 250)); }
        // The CTA is the EXACT button #gen-text-<id> — not `/transcribe/i` over the
        // whole body text, which any nav/settings copy could satisfy.
        const cta = !!document.querySelector(`#gen-text-${w.id}`);
        return { playing: !!(el && !el.paused && el.currentTime > 1), cta, waits: { 'audio-playing': attempts <= 40 ? attempts : -40 } };
      }, WORK).catch(e => ({ playing: false, cta: false, why: String(e).slice(0, 80) }));
      for (const [k, v] of Object.entries(r.waits || {})) record(k, v > 0 ? 'ready' : 'TIMEOUT', Math.abs(v), Math.abs(v) * 250);
      report('pretranscribe_play', !!(r.playing && r.cta),
        `playing=${r.playing} transcribeCTA(#gen-text-${WORK})=${r.cta}${r.why ? ' err=' + r.why : ''}`);
    }
    console.log(`
--- SHAPE: ${kind} — standard audio+text journeys are n/a by shape (asserted above instead) ---`);
    process.exit(finish());
  }

  // ---- play_and_hear: open the READER overlay on a mid-book (narrated) chapter,
  // then start playback there — the karaoke .sync-word spans live in the reader
  // overlay, and front-matter chapters have no audio sync. Pick the displayed
  // text source + a chapter past the front matter.
  // openWorkDetail shows the work page but does NOT open the reader overlay, so
  // no word map (activeSyncData) exists yet. Two stages:
  //   A. seek near the start to OPEN the reader + load a word map + start audio.
  //   B. re-seat the audio to a point INSIDE that loaded map (~30% through its own
  //      words) so reader and audio sit on the SAME span. The false positive was a
  //      FIXED 20%-of-total-duration seek (1644s) landing OUTSIDE the loaded map
  //      (~52s) — audio ran, but the active word never moved. Driving the audio to
  //      the loaded text is exactly the invariant the product must hold.
  await page.evaluate((wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    if (typeof seekToAbsoluteBookTime === 'function') seekToAbsoluteBookTime(w, 20, w.title);
    const a = document.getElementById('audio-player');
    if (a && a.paused) a.play().catch(() => {});
  }, WORK).catch(driveErr('play_and_hear', 'seek-open-reader'));
  // Stage A readiness = stage A's OWN outcome has landed: the book clock sits at the
  // 20s seek AND the reader overlay has a word map. "Map loaded" alone fired ~270ms in,
  // while stage A's async seek (chapter-list load → chapter follow → play@20s) was
  // still in flight; stage B's seek then raced it and the SLOWER stage-A seek resolved
  // last and won — the audio played from ~20s and the seeded 697s never landed (live
  // work 85; the old runner's 2200ms sleep let stage A finish first). Two concurrent
  // drives on one player: wait for the first to land before issuing the second.
  await waitFor('seek-landed@20s (stage A) + word map', async () => {
    const c = await uiClockRaw();
    const mapped = await page.evaluate(() => typeof activeSyncData !== 'undefined' && activeSyncData && activeSyncData.length > 0
      && !!(document.getElementById('audio-player') || {}).src).catch(() => false);
    return mapped && c != null && c >= 17 && c <= 65;
  }, { timeoutMs: 20000, soft: true });
  const seedTarget = await page.evaluate((wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    const sd = (typeof activeSyncData !== 'undefined' && activeSyncData) ? activeSyncData : null;
    let seededAt = null;
    if (sd && sd.length) {
      // Seed with RUNWAY. The karaoke checks play ~18s after this seed; if the
      // loaded chapter's map is short (e.g. a 25s title page) a fixed 30%-in seed
      // overflows the chapter end and the highlight freezes at the last word — a
      // harness false-red, not a product stall (that class is resume_flow's).
      // Target ~30% in but never within RUNWAY secs of the map end, never before a
      // small head offset. Seek to the word nearest that time.
      const RUNWAY = 22;
      const first = sd[0].s, last = sd[sd.length - 1].s, span = last - first;
      let target = first + span * 0.3;
      target = Math.min(target, last - RUNWAY);   // keep runway before the chapter ends
      target = Math.max(target, first + 2);        // ...but past the very first word
      if (target > last) target = first + Math.min(2, span / 2); // ultra-short map: just after start
      let idx = 0, bestD = Infinity;
      for (let i = 0; i < sd.length; i++) { const d = Math.abs(sd[i].s - target); if (d < bestD) { bestD = d; idx = i; } }
      const at = sd[idx].s;
      seededAt = at;
      if (typeof seekToAbsoluteBookTime === 'function') seekToAbsoluteBookTime(w, at, w.title);
      else { const a = document.getElementById('audio-player'); if (a) a.currentTime = at; }
    }
    const a = document.getElementById('audio-player');
    if (a && a.paused) a.play().catch(() => {});
    return seededAt;
  }, WORK).catch(driveErr('play_and_hear', 'seek-into-map'));
  // Stage B readiness = the seek's OUTCOME is observable: the book clock sits at the
  // seeded target (seekToAbsoluteBookTime is async — chapter follow, file load, then
  // the seek). The first conversion waited for "audio playing", which was ALREADY
  // true from stage A, so on the live server the 6s measurement started before the
  // seek resolved (clock 20→25s instead of ~700s; the small fixture files landed
  // inside one poll interval and hid it). A readiness condition for a measurement
  // must be the measurement's PRECONDITION, not any state that happens to be true.
  if (seedTarget != null) {
    await waitFor(`seek-landed@${Math.round(seedTarget)}s`, async () => {
      const c = await uiClockRaw(); return c != null && c >= seedTarget - 3 && c <= seedTarget + 45;
    }, { timeoutMs: 20000, soft: true });
  } else {
    await waitFor('audio-playing (no map to seed into)', () => page.evaluate(() => {
      const a = document.getElementById('audio-player'); return !!(a && !a.paused && a.currentTime > 0);
    }), { timeoutMs: 15000, soft: true });
  }
  const t0 = clockSecs(await page.locator('.player-time').first().textContent().catch(() => null));
  await page.waitForTimeout(6000);
  const t1 = clockSecs(await page.locator('.player-time').first().textContent().catch(() => null));
  const heard = t0 != null && t1 != null && t1 - t0 >= 3;
  report('play_and_hear', heard, `clock ${t0}s -> ${t1}s over 6s wall`);
  if (!heard) await page.screenshot({ path: `${SHOTS}/play_and_hear-FAIL.png` });

  // ---- karaoke_advances: A1-A5
  async function karaokeState() {
    return page.evaluate(() => {
      const words = document.querySelectorAll('.sync-word');
      const read = [...document.querySelectorAll('.sync-word.read')]
        .map(e => +e.dataset.widx).filter(n => !isNaN(n));
      const active = read.length ? Math.min(...read) + 1 : null;
      const map = (typeof activeSyncData !== 'undefined' && activeSyncData) ? activeSyncData : null;
      return {
        wordCount: words.length,
        active,
        activeMapSec: (map && active != null && map[active]) ? map[active].s : null,
        mapLen: map ? map.length : 0,
        mapMinSec: (map && map.length) ? map[0].s : null,
        mapMaxSec: (map && map.length) ? map[map.length - 1].s : null,
      };
    });
  }
  const uiClock = async () => clockSecs(await page.locator('.player-time').first().textContent().catch(() => null));

  // DEGRADE-AWARE SETTLE (calibrated 2026-08-10, after defect A landed —
  // held for three rounds so we would calibrate against corrected behaviour,
  // not against the bug): on a weak-chain work the ebook honestly renders
  // PLAIN text (mode=none) until a degrading chapter loads, so zero
  // .sync-word here is not a karaoke failure — it is the reader not yet
  // walked to a chapter a person would read. Walk it: open the displayed
  // text's chapter 2 through the product's own loadChapter (this is what
  // triggers the degrade switch to the transcript), then wait for word
  // spans to settle. Strong-chain fixtures are unaffected (words are
  // already on screen and the walk is skipped). A1-A5 still assert REAL
  // advancing karaoke on whatever book the product honestly displays.
  {
    const s0 = await karaokeState();
    if (s0.wordCount === 0) {
      await page.evaluate(async (wid) => {
        const w = (allWorks || []).find(x => x.id === Number(wid));
        const tf = (typeof displayEditionBooks === 'function') ? displayEditionBooks(w, 'text')[0] : (w.text_files || [])[0];
        if (tf && typeof loadChapter === 'function') await loadChapter(tf.id, 2, w.id);
      }, WORK).catch(driveErr('karaoke_advances', 'degrade-walk-loadChapter'));
      await waitFor('sync-words-rendered', async () => (await karaokeState()).wordCount > 0, { timeoutMs: 8000, intervalMs: 500, soft: true });
    }
  }
  // CONVERSION DEBT ITEM (RUNNER-CONTRACT.md): the s1 sample used to fire straight
  // after blind settles, so under load the reader/sync had not finished mounting →
  // active=null → a FALSE red. Wait for the karaoke to actually be LIT (a .sync-word
  // has the .read class) before opening the 10s measurement window. A timeout here
  // is recorded and the window still runs — A2/A3 then fail HONESTLY on a highlight
  // that never lit, which is the product state, not a race.
  await waitFor('active-word-lit', async () => (await karaokeState()).active != null, { timeoutMs: 15000, soft: true });
  const s1 = await karaokeState(); const c1 = await uiClock(); const w1 = Date.now();
  await page.waitForTimeout(10000);
  const s2 = await karaokeState(); const c2 = await uiClock(); const w2 = Date.now();

  const a1 = s2.wordCount >= 50;
  const a2 = s2.active != null;
  const a3 = a2 && s1.active != null && s2.active > s1.active;
  const a4 = s2.activeMapSec != null && c2 != null && Math.abs(s2.activeMapSec - c2) <= 2.5;
  const wall = (w2 - w1) / 1000;
  const a5 = c1 != null && c2 != null && Math.abs((c2 - c1) - wall) <= 2.5;
  const ok = a1 && a2 && a3 && a4 && a5;
  report('karaoke_advances', ok,
    `A1 words=${s2.wordCount} A2 active=${s2.active} A3 ${s1.active}->${s2.active} ` +
    `A4 |map ${s2.activeMapSec == null ? 'null' : s2.activeMapSec.toFixed(1)} - clock ${c2}| ` +
    `A5 clock +${c1 != null && c2 != null ? (c2 - c1).toFixed(1) : '?'}s over ${wall.toFixed(1)}s`);
  if (!ok) await page.screenshot({ path: `${SHOTS}/karaoke-FAIL.png` });
  // Record WHICH chapter/window the karaoke checks actually watched, so the
  // limits block can say what the green (or red) applies to.
  coverage.karaokeChapter = await page.evaluate((wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    return (typeof currentReaderChapter !== 'undefined' && w && currentReaderChapter[w.id]) ? currentReaderChapter[w.id].index : null;
  }, WORK).catch(() => null);
  coverage.karaokeWords = s2.wordCount;
  coverage.karaokeAudioSec = c2;

  // ---- change_chapter: load a DIFFERENT chapter in the open reader; content changes.
  // Pause first so the audio-follow sync doesn't immediately pull the reader back
  // to the playing chapter.
  await page.evaluate(() => { const a = document.getElementById('audio-player'); if (a && !a.paused) a.pause(); }).catch(driveErr('change_chapter', 'pause'));
  await waitFor('audio-paused', () => page.evaluate(() => { const a = document.getElementById('audio-player'); return !a || a.paused; }), { timeoutMs: 5000, soft: true });
  // Assert on the reader's CHAPTER STATE, not text-content diff: a Gutenberg EPUB
  // has near-identical front-matter sections, so two chapters can look the same
  // even when the load worked. currentReaderChapter[work].index is the truth.
  const chg = await page.evaluate(async (wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    const tf = (typeof displayEditionBooks === 'function') ? displayEditionBooks(w, 'text')[0] : (w.text_files || [])[0];
    // chapterCache is a top-level `const` in index.html — accessible as a bare
    // identifier in evaluate (like allWorks/activeSyncData), NOT as window.chapterCache
    // (that reads undefined and made chs always []). Use the bare binding.
    const cc = (typeof chapterCache !== 'undefined') ? chapterCache : {};
    if (!(cc[tf.id]?.chapters?.length) && typeof loadChapterList === 'function') {
      await loadChapterList(tf.id, w.id);
    }
    const chs = (cc[tf.id]?.chapters) || [];
    if (chs.length < 2) return { ok: false, why: `only ${chs.length} chapters in book ${tf.id}` };
    const cur = (typeof currentReaderChapter !== 'undefined' && currentReaderChapter[w.id]) ? currentReaderChapter[w.id].index : chs[0].index;
    const other = chs.find(c => c.index !== cur) || chs[chs.length - 1];
    await loadChapter(tf.id, other.index, w.id);
    let attempts;
    for (attempts = 1; attempts <= 40; attempts++) {
      const n = (typeof currentReaderChapter !== 'undefined' && currentReaderChapter[w.id]) ? currentReaderChapter[w.id].index : cur;
      if (n === other.index) break;
      await new Promise(r => setTimeout(r, 250));
    }
    const now = (typeof currentReaderChapter !== 'undefined' && currentReaderChapter[w.id]) ? currentReaderChapter[w.id].index : cur;
    return { ok: now === other.index && now !== cur, from: cur, to: now, wanted: other.index, waits: { 'reader-chapter-changed': attempts <= 40 ? attempts : -40 } };
  }, WORK).catch((e) => ({ ok: false, why: String(e) }));
  for (const [k, v] of Object.entries(chg.waits || {})) record(k, v > 0 ? 'ready' : 'TIMEOUT', Math.abs(v), Math.abs(v) * 250);
  report('change_chapter', chg.ok, chg.ok ? `chapter ${chg.from}->${chg.to}` : (chg.why || `stayed on ${chg.from}`));

  // ---- resume_flow: the stuck-'Cratchit' catch. PJ's most damning screenshot —
  // the highlight frozen on "Cratchit ,," at a chapter's END while the audio played
  // on ELSEWHERE. It is the report that most looks like the product simply not
  // working, because the words are visibly there and visibly wrong.
  //
  // THIS IS A RESUME JOURNEY, NOT A PLAY-THROUGH — do NOT "simplify" it to play
  // from 0:00. The bug only appears when playback picks up somewhere OTHER than the
  // start (the failure lives at the transition, same lesson as mobile's
  // auto-advance). We resume from the work's SAVED position via the product's own
  // resumeOrPlay path (messy 8198 carries an imported position at 1591.8s, mid
  // Stave Two). PASS = the reader navigates to the chapter the audio is in — the
  // audio clock falls INSIDE the shown chapter's word-map extent. FAIL signatures:
  //   * "stuck": words>0 but the clock is PAST the shown chapter's whole map — the
  //     reader did NOT auto-navigate to the narrated chapter. THIS IS PJ'S BUG, real.
  //   * "harness": words==0 / reader never opened while the clock advances — fix the
  //     runner's resume drive, not the product.
  // Reload FIRST so the audio player is empty — otherwise this goto's pagehide
  // fires flushPositionBeacon(), which re-saves the shallow position still loaded
  // from play_and_hear and CLOBBERS the deep position we are about to plant.
  // 'networkidle' NEVER fires against a busy live server (WS + job-poll + covers keep
  // the network alive) — it FATAL'd open_library the same way (5d46ef0). Same fix.
  await page.goto(BASE, { waitUntil: 'domcontentloaded' });
  await waitFor('library-works-loaded (resume reload)', libraryLoaded, { timeoutMs: 30000, intervalMs: 300, soft: true });
  // PRECONDITION: plant a DEEP saved position, because the fixture's imported one
  // (1591.8s) is clobbered by any prior playback autosave — including this suite's
  // own earlier journeys. resume_flow owns its precondition so it is deterministic
  // regardless of run order. Plant AFTER the reload (no live player to re-clobber),
  // ~50% through the book (mid a later file), then resume via the real resumeOrPlay
  // with NO further reload between plant and resume.
  const planted = await page.evaluate(async (wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    // Plant INSIDE THE DISPLAY EDITION's timeline. Planting over ALL audio_files put
    // the position into the OTHER edition on a two-edition work (85: file idx6 of 11
    // = the human narration while the TTS edition is displayed), so "resume" landed
    // at 475s of a 9372s plant and the map-extent check passed by coincidence — an
    // assertion too weak to fail (RULE 2). The recorded resume-landed TIMEOUT is
    // what exposed it.
    const files = (typeof displayEditionBooks === 'function') ? displayEditionBooks(w, 'audio') : (w.audio_files || []);
    if (!files.length) return null;
    const total = files.reduce((s, f) => s + (f.duration_secs || 0), 0);
    const target = total * 0.5;
    let acc = 0, book = files[0], idx = 0, local = 0;
    for (let i = 0; i < files.length; i++) {
      const d = files[i].duration_secs || 0;
      if (acc + d >= target) {
        book = files[i]; idx = i; local = Math.round(target - acc);
        // Never plant within 30s of a file EDGE: 50% of an 8×1h edition landed on the
        // LAST second of file 3, where the player's auto-advance re-rendered the
        // reader between the harness poll and the assert (words 8321 → 0) — a
        // boundary artifact of the plant, not the resume path. A precondition must be
        // unambiguous.
        if (d > 90) local = Math.min(Math.max(local, 30), Math.round(d) - 30);
        break;
      }
      acc += d;
    }
    const globalApprox = acc + local;
    // The plant is the journey's PRECONDITION — a failed POST is recorded, not dropped.
    let postStatus = 0;
    await fetch(`/api/works/${w.id}/position`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ book_id: book.id, file_index: idx, position_secs: local }),
    }).then(r => { postStatus = r.status; }).catch(() => { postStatus = -1; });
    return { bookId: book.id, fileIdx: idx, local, globalApprox: Math.round(globalApprox), postStatus };
  }, WORK).catch(driveErr('resume_flow', 'plant-position'));
  if (planted && !(planted.postStatus >= 200 && planted.postStatus < 300)) driveErr('resume_flow', 'plant-position POST')(`status ${planted.postStatus}`);
  await page.evaluate((wid) => { if (typeof openWorkDetail === 'function') openWorkDetail(Number(wid)); }, WORK).catch(driveErr('resume_flow', 'openWorkDetail'));
  await waitFor('work-detail-rendered (resume)', detailRendered, { timeoutMs: 15000, soft: true });
  await page.evaluate((wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    if (w && typeof resumeOrPlay === 'function') resumeOrPlay(w.id); // resumes at the DEEP planted position
    const a = document.getElementById('audio-player');
    if (a && a.paused) a.play().catch(() => {});
  }, WORK).catch(driveErr('resume_flow', 'resumeOrPlay'));
  // Readiness = the resume's OUTCOME: the reader opened with words AND the book clock
  // has landed at the planted position (resumeOrPlay is async: fetch position → load
  // file → seek). "Clock readable" alone was true the instant the file loaded at
  // 0:00, before the seek — the live server then read clock=0 and cried "stuck"
  // (a harness false-red of PJ's real bug class). SOFT — on a timeout the journey's
  // own signatures ("harness", "stuck", n/a on incoherent data) read the honest
  // state; the timeout is in the artifacts.
  const plantedGlobal = planted ? planted.globalApprox : null;
  await waitFor(plantedGlobal != null ? `resume-landed@~${plantedGlobal}s` : 'resume-reader-open', async () => {
    const k = await karaokeState(); const c = await uiClock();
    if (!(k.wordCount > 0 && c != null)) return false;
    return plantedGlobal == null || Math.abs(c - plantedGlobal) <= 90; // planted is a per-file rounding; 90s slack
  }, { timeoutMs: 20000, soft: true });
  const rs = await karaokeState();
  const rc = await uiClock();
  // Is the follow even certifiable on this work? Reader-follow needs a REAL
  // narration↔text mapping. A word-anchor ebook with NO word alignment shows a
  // SYNTHETIC (uniform) map — there is nothing to follow, so a "stuck" reading is
  // a FIXTURE property, not the product bug. Distinguish it (else the journey
  // cries wolf on an unaligned work, e.g. clean 8199 with pairs:[]).
  const ctx2 = await page.evaluate(async (wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    const isEbookKaraoke = (typeof currentReaderIsEbookKaraoke !== 'undefined') ? currentReaderIsEbookKaraoke : false;
    let wordPairs = 0, coherent = true;
    try {
      const cov = await fetch(`/api/works/${w.id}/coverage`).then(r => r.json());
      wordPairs = (cov.pairs || []).filter(p => p.unit === 'word').length;
    } catch {}
    try {
      const canon = await fetch(`/api/works/${w.id}/canon`).then(r => r.ok ? r.json() : null);
      if (canon && canon.coherent === false) coherent = false;
    } catch {}
    return { isEbookKaraoke, wordPairs, coherent };
  }, WORK).catch(() => ({ isEbookKaraoke: false, wordPairs: 0, coherent: true }));
  // Reader followed iff the audio clock sits within the shown chapter's map extent.
  // Using the map EXTENT (not the highlighted word) avoids a false "harness" verdict
  // on a resume that hasn't reached its first word yet.
  const withinChapter = rs.mapMaxSec != null && rs.mapMinSec != null && rc != null
    && rc <= rs.mapMaxSec + 2.5 && rc >= rs.mapMinSec - 2.5;
  const notApplicable = ctx2.isEbookKaraoke && ctx2.wordPairs === 0; // synthetic map, nothing to follow
  const blockedByIncoherent = rs.wordCount === 0 && !ctx2.coherent; // resume can't build a coherent map on incoherent data
  // The resume must also LAND where it was planted (±90s: the plant is a per-file
  // rounding and the clock has been running). Without this, a resume that played
  // from the wrong place but happened to sit inside SOME chapter's map passed.
  const landedNearPlant = planted == null || (rc != null && Math.abs(rc - planted.globalApprox) <= 90);
  const rFollows = notApplicable || blockedByIncoherent || (rs.wordCount >= 50 && withinChapter && landedNearPlant);
  const plantStr = planted ? `planted@~${planted.globalApprox}s(book ${planted.bookId} idx${planted.fileIdx}+${planted.local}s)` : 'plant FAILED';
  let rsig;
  if (notApplicable) rsig = `n/a: display ebook has NO word alignment (synthetic map) — reader-follow not certifiable on this work; give it a real alignment to test`;
  else if (blockedByIncoherent) rsig = `n/a: canon.coherent=false — resume cannot build a coherent map on multi-edition-split data; the ROOT is surface_consistency's, not this drive`;
  else if (rs.wordCount === 0) rsig = `harness: reader never opened (words=0) while clock=${rc} [${plantStr}] — fix the resume drive`;
  else if (!withinChapter) rsig = `stuck: clock=${rc} is PAST the shown chapter's map [${rs.mapMinSec == null ? '?' : rs.mapMinSec.toFixed(0)}..${rs.mapMaxSec == null ? '?' : rs.mapMaxSec.toFixed(0)}s] [${plantStr}] — reader did NOT follow audio (PJ's 'Cratchit')`;
  else if (!landedNearPlant) rsig = `resumed ELSEWHERE: clock=${rc} vs ${plantStr} (Δ${Math.round(rc - planted.globalApprox)}s) — the saved position was not honoured`;
  else rsig = `reader followed: clock=${rc} within map [${rs.mapMinSec.toFixed(0)}..${rs.mapMaxSec.toFixed(0)}s] and ${Math.round(rc - planted.globalApprox)}s from the plant, ${rs.wordCount} words [${plantStr}]`;
  report('resume_flow', rFollows, rsig);
  if (!rFollows) await page.screenshot({ path: `${SHOTS}/resume_flow-FAIL.png` });

  // ---- switch_source: EVERY text source must render real content for a mid-book
  // chapter. This is the one that catches a source rendering almost nothing —
  // Carol's transcript today ("This is", then nothing). A source under the floor
  // fails LOUDLY instead of passing because the OTHER (good) source was showing.
  // For EACH source, check TWO things: (a) it renders content when switched to,
  // and (b) it has no near-empty INTERIOR chapters — the failure that renders
  // "This is" and nothing when the reader lands on one. Sampling a single
  // mid-book chapter is too weak (Carol's transcript alternates 2-word fragments
  // with real chapters; a mid pick can hit a good one). So scan every chapter's
  // rendered length across each source.
  const srcResult = await page.evaluate(async (wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    const texts = (w.text_files || []).filter(b => b.visibility !== 'internal');
    const out = [];
    for (const tf of texts) {
      const chs = await fetch(`/api/books/${tf.id}/chapters`).then(r => r.json()).catch(() => []);
      const list = Array.isArray(chs) ? chs : (chs.chapters || []);
      // Interior chapters (drop first + last: legit short front/back matter).
      const interior = list.slice(1, -1);
      const nearEmpty = interior.filter(c => (c.word_count || 0) < 10).length;
      const total = list.reduce((s, c) => s + (c.word_count || 0), 0);
      out.push({ fmt: tf.format, chapters: list.length, nearEmptyInterior: nearEmpty, totalWords: total });
    }
    return out;
  }, WORK).catch(() => []);
  // A source is broken if it holds real content overall yet has interior chapters
  // that render essentially nothing — a reader landing on one sees "This is".
  const broken = srcResult.filter(s => s.totalWords > 200 && s.nearEmptyInterior > 0);
  coverage.sources = srcResult;
  report('switch_source', srcResult.length > 0 && broken.length === 0,
    srcResult.map(s => `${s.fmt}:${s.chapters}ch/${s.nearEmptyInterior}empty`).join(' ') || 'no text sources');
  if (broken.length) await page.screenshot({ path: `${SHOTS}/switch_source-FAIL.png` });

  // ---- surface_consistency (cross-surface assert, opened 2026-08-09;
  // transcription owns the contract: GET /api/works/{id}/canon). web ==
  // mobile == canon, so ONE canonical answer arbitrates all three surfaces.
  // Red path 1: canon.coherent=false — the DATA itself cannot be presented
  // consistently (PJ's edition-label split: one narration split across two
  // edition labels); no renderer can be right, so every surface must fail.
  // Red path 2: the web's OWN rendered numbers disagree with canon.active.
  // Mobile asserts its numbers against the same canon in its runner.
  try {
    const canon = await page.evaluate(async (wid) => {
      const r = await fetch(`/api/works/${wid}/canon`);
      return r.ok ? r.json() : null;
    }, WORK);
    if (!canon) {
      report('surface_consistency', false, 'canon endpoint missing/unreadable (transcription owns GET /api/works/{id}/canon)');
    } else if (!canon.coherent) {
      // The data can't be shown consistently — fail loudly on the root, not a symptom.
      report('surface_consistency', false, 'DATA INCOHERENT (no surface can be right): ' + (canon.problems || []).join(' | '));
    } else {
      // Coherent data: assert the numbers the WEB ACTUALLY RENDERS (via the app's
      // own displayEditionBooks edition resolution + the DOM count spans — NOT a
      // body-text regex, which the current UI's "Audiobook · N chapters" wording
      // doesn't match) equal the canonical active edition. This is server-web's
      // half: web number == canon; mobile pins its number to the same canon.
      const web = await page.evaluate((wid) => {
        const w = (allWorks || []).find(x => x.id === Number(wid));
        const da = (typeof displayEditionBooks === 'function') ? displayEditionBooks(w, 'audio') : (w.audio_files || []);
        const dt = (typeof displayEditionBooks === 'function') ? displayEditionBooks(w, 'text') : (w.text_files || []);
        const span = document.getElementById('audio-count-' + w.id);
        return {
          audioFiles: da.length,
          audioSpan: span ? parseInt(span.textContent, 10) : null, // the rendered "N chapters" count
          textChapters: dt[0] ? dt[0].chapter_count : null,
          textBookId: dt[0] ? dt[0].id : null,
        };
      }, WORK);
      // PROJECTION LAW: the span renders CHAPTERS, so it asserts against
      // canon.active.audio_chapters — comparing it to audio_files went red on
      // agreeing surfaces (5-file human edition, 6 detected chapters).
      const okAudio = web.audioFiles === canon.active.audio_files
        && (web.audioSpan == null || web.audioSpan === canon.active.audio_chapters);
      const okText = web.textChapters == null || web.textChapters === canon.active.text_chapters;
      report('surface_consistency', okAudio && okText,
        `web renders ${web.audioFiles} audio files (span=${web.audioSpan}ch) / ${web.textChapters} text-ch ` +
        `vs canon.active ${canon.active.audio_files} files/${canon.active.audio_chapters}ch / ${canon.active.text_chapters} text-ch ` +
        `(totals ${canon.total_audio_files}A/${canon.total_texts}T)`);
    }
  } catch (e) {
    report('surface_consistency', false, 'error: ' + e.message.slice(0, 120));
  }

  // ---- narration_binding (multi-edition fix, authorized 2026-08-09): the word map
  // the reader follows must be timed to the NARRATION ACTUALLY PLAYING. A work with
  // both a TTS edition (word-perfect by construction) AND a human recording
  // (anchor-aligned, timed to its OWN pacing) carries two distinct timelines for the
  // same EPUB. Returning the anchor map while the TTS narration plays is the desync
  // PJ saw: right chapter, wrong sentence. The endpoint takes ?audio={audioBookId};
  // this asserts the two narrations yield DISTINCT maps (server bound them). n/a for
  // single-narration works. RED when identical = ?audio ignored (pre-fix), so
  // playing the TTS narration would show the human narration's times.
  try {
    const nb = await page.evaluate(async (wid) => {
      const w = (allWorks || []).find(x => x.id === Number(wid));
      const audio = (w.audio_files || []);
      const tts = audio.find(b => b.origin === 'tts_kokoro');
      const human = audio.find(b => b.origin === 'narrator_recording' || b.origin === 'author_recording' || b.origin === 'librivox');
      const epub = (typeof displayEditionBooks === 'function')
        ? displayEditionBooks(w, 'text').find(b => b.format === 'epub')
        : (w.text_files || []).find(b => b.format === 'epub');
      if (!tts || !human || !epub) return { na: true, why: `single-narration (tts=${!!tts} human=${!!human} epub=${!!epub})` };
      const chs = await fetch(`/api/books/${epub.id}/chapters`).then(r => r.json()).catch(() => []);
      const list = Array.isArray(chs) ? chs : [];
      const ch = list.filter(c => (c.word_count || 0) > 200).sort((a, b) => (b.word_count || 0) - (a.word_count || 0))[0]
        || list[Math.floor(list.length / 2)];
      if (!ch) return { na: true, why: 'no substantial epub chapter' };
      const ext = async (aud) => {
        const m = await fetch(`/api/works/${w.id}/word-sync/${epub.id}/${ch.index}?audio=${aud}`).then(r => r.json()).catch(() => []);
        return (Array.isArray(m) && m.length) ? { n: m.length, first: m[0].s, last: m[m.length - 1].s } : null;
      };
      return { na: false, chIndex: ch.index, ttsMap: await ext(tts.id), humanMap: await ext(human.id) };
    }, WORK);
    if (nb.na) {
      report('narration_binding', true, `n/a: ${nb.why} — nothing to bind`);
    } else if (!nb.ttsMap || !nb.humanMap) {
      report('narration_binding', false, `a narration returned NO map (tts=${!!nb.ttsMap} human=${!!nb.humanMap}) — binding/alignment gap`);
    } else {
      // Distinct timelines per narration => the server honoured ?audio. Identical =>
      // it ignored ?audio and served the same (anchor) map for both = the desync.
      const distinct = Math.abs(nb.ttsMap.first - nb.humanMap.first) > 2 || Math.abs(nb.ttsMap.last - nb.humanMap.last) > 5;
      report('narration_binding', distinct,
        `ch${nb.chIndex}: TTS map [${nb.ttsMap.first.toFixed(0)}..${nb.ttsMap.last.toFixed(0)}s] vs human [${nb.humanMap.first.toFixed(0)}..${nb.humanMap.last.toFixed(0)}s] — ` +
        (distinct ? 'BOUND (distinct per narration)' : 'NOT BOUND (identical → ?audio ignored; playing TTS would show human times)'));
    }
  } catch (e) {
    report('narration_binding', false, 'error: ' + e.message.slice(0, 120));
  }

  // ---- timing_basis (TIMING LAW, META d-sw-gatearmed pt.4): every sync map
  // SELF-DESCRIBES the narration it times (basis.audio_book_ids); a map fetched for a
  // given ?edition MUST be timed to THAT edition; and with no ?edition on a
  // multi-edition work the server resolves to canon.active and SAYS SO
  // (basis.resolved). This turns the exact failure PJ photographed — the reader
  // following one narration's timings while another plays — into a PERMANENT red, not
  // a fix for one book. n/a for single-edition works (nothing to disambiguate).
  try {
    const tb = await page.evaluate(async (wid) => {
      const w = (allWorks || []).find(x => x.id === Number(wid));
      const canon = await fetch(`/api/works/${wid}/canon`).then(r => r.json()).catch(() => ({}));
      const eds = canon.editions || [];
      if (eds.length < 2) return { na: true, why: `single edition (${eds.length})` };
      const epub = (w.text_files || []).find(b => b.format === 'epub');
      if (!epub) return { na: true, why: 'no epub' };
      const chs = await fetch(`/api/books/${epub.id}/chapters`).then(r => r.json()).catch(() => []);
      const ch = (Array.isArray(chs) ? chs : []).filter(c => (c.word_count || 0) > 200)[0];
      if (!ch) return { na: true, why: 'no substantial chapter' };
      const basisFor = async (bookId) => {
        const q = bookId ? `?edition=${bookId}` : '';
        const ts = await fetch(`/api/works/${wid}/text-sync/${epub.id}/${ch.index}${q}`).then(r => r.json()).catch(() => ({}));
        return ts.basis || null;
      };
      return {
        na: false, ed0: eds[0].book_ids[0], ed1: eds[1].book_ids[0], activeDir: canon.active.edition_dir,
        b0: await basisFor(eds[0].book_ids[0]), b1: await basisFor(eds[1].book_ids[0]), bDefault: await basisFor(0),
      };
    }, WORK);
    if (tb.na) {
      report('timing_basis', true, `n/a: ${tb.why} — nothing to disambiguate`);
    } else {
      const names = (basis, id) => !!(basis && (basis.audio_book_ids || []).includes(id));
      const ok0 = names(tb.b0, tb.ed0);        // map fetched FOR edition0 names edition0
      const ok1 = names(tb.b1, tb.ed1);        // ...and edition1 names edition1
      const distinct = tb.b0 && tb.b1 && tb.b0.edition_dir !== tb.b1.edition_dir; // different narrations
      const announced = !!(tb.bDefault && tb.bDefault.resolved === true && tb.bDefault.edition_dir === tb.activeDir);
      report('timing_basis', ok0 && ok1 && distinct && announced,
        `basis self-describes: ed0-names-ed0=${ok0}, ed1-names-ed1=${ok1}, distinct=${distinct}; ` +
        `no-param default = canon.active + announced(resolved)=${announced}`);
    }
  } catch (e) {
    report('timing_basis', false, 'error: ' + e.message.slice(0, 120));
  }

  // ---- weak_chain_degrade (authorized 2026-08-09): when the printed edition's
  // chain to the human narration is too weak to trust a word highlight, the reader
  // must DEGRADE HONESTLY — actively show the TRANSCRIPT (synced to the narrator)
  // with a plain-words note, never a confident wrong highlight nor a silent "no
  // sync". This leg proves the degraded state is REACHED (reader switches to the
  // transcript) and RENDERS (the note is visible), not merely that the threshold
  // computes. n/a when the work has no weak-chain ebook to exercise it.
  try {
    const wc = await page.evaluate(async (wid) => {
      const w = (allWorks || []).find(x => x.id === Number(wid));
      const human = (w.audio_files || []).find(b => b.origin === 'narrator_recording' || b.origin === 'author_recording' || b.origin === 'librivox');
      const epub = (w.text_files || []).find(b => b.format === 'epub');
      const trans = (w.text_files || []).find(b => b.format === 'transcript');
      if (!human || !epub || !trans) return { na: true, why: 'needs epub + human narration + transcript' };
      // SCAN the epub's content chapters for the FIRST that actually degrades on
      // the human narration (a word map that fails the confidence gate). Front
      // matter has no word map and never degrades — testing a fixed index would
      // pick it and miss the real path. n/a when no chapter degrades (strong chain
      // or all-paragraph epub).
      const chs = await fetch(`/api/books/${epub.id}/chapters`).then(r => r.json()).catch(() => []);
      const content = (Array.isArray(chs) ? chs : []).filter(c => (c.word_count || 0) > 200);
      let ch = null, ts = null;
      for (const c of content) {
        const t = await fetch(`/api/works/${w.id}/text-sync/${epub.id}/${c.index}?audio=${human.id}`).then(r => r.json()).catch(() => ({}));
        if (t && t.degrade_to) { ch = c; ts = t; break; }
      }
      if (!ch) return { na: true, why: 'no degrading word chapter on this epub (strong chain or paragraph-only)' };
      // Weak: drive the reader — play the human narration, open the degrading chapter.
      playAudio(human.id, 0, w.id, human.title || human.filename, w.title, 120);
      window.__wc = { epubId: epub.id, chIndex: ch.index, transId: trans.id, degradeTo: ts.degrade_to, note: ts.degrade_note };
      return { na: false, driving: true };
    }, WORK);
    if (wc.na) {
      report('weak_chain_degrade', true, `n/a: ${wc.why}`);
    } else {
      await waitFor('human-narration-playing', () => page.evaluate(() => { const a = document.getElementById('audio-player'); return !!(a && !a.paused && a.currentTime > 0); }), { timeoutMs: 15000, soft: true });
      await page.evaluate(() => { const w = allWorks.find(x => x.id === currentWorkId); loadChapter(window.__wc.epubId, window.__wc.chIndex, w.id, { skipAudioSeek: true }); }).catch(driveErr('weak_chain_degrade', 'loadChapter'));
      await waitFor('reader-degraded-to-transcript', () => page.evaluate((wid) => {
        const rc = (typeof currentReaderChapter !== 'undefined') ? currentReaderChapter[Number(wid)] : null;
        const note = document.getElementById('reader-degrade-note');
        return !!(rc && rc.bookId === window.__wc.degradeTo && note && note.offsetHeight > 0);
      }, WORK), { timeoutMs: 15000, soft: true });
      const rr = await page.evaluate((wid) => {
        const rc = (typeof currentReaderChapter !== 'undefined') ? currentReaderChapter[Number(wid)] : null;
        const note = document.getElementById('reader-degrade-note');
        return {
          readerBookId: rc ? rc.bookId : null,
          degradeTo: window.__wc.degradeTo,
          noteVisible: note ? (note.style.display !== 'none' && note.offsetHeight > 0) : false,
          noteMatches: note ? (note.textContent || '').trim() === (window.__wc.note || '').trim() : false,
        };
      }, WORK);
      const reached = rr.readerBookId === rr.degradeTo;   // switched to the transcript
      const renders = rr.noteVisible && rr.noteMatches;    // note is visible + correct
      report('weak_chain_degrade', reached && renders,
        `reader ${reached ? 'SWITCHED to transcript' : `stayed on ${rr.readerBookId} (wanted ${rr.degradeTo})`}; ` +
        `note ${renders ? 'VISIBLE' : `NOT rendered (visible=${rr.noteVisible} matches=${rr.noteMatches})`}`);
      if (!(reached && renders)) await page.screenshot({ path: `${SHOTS}/weak_chain_degrade-FAIL.png` });
    }
  } catch (e) {
    report('weak_chain_degrade', false, 'error: ' + e.message.slice(0, 120));
  }

  await browser.close();
  process.exit(finish());
})().catch(e => { console.error('FATAL', e); process.exit(2); });

function finish() {
  const failed = results.filter(r => !r.ok);
  // ---- WHAT THIS RUN CAN AND CANNOT SEE ----------------------------------
  // A green run must be green about something specific. State the scope that
  // was actually watched and the two fault classes these DOM checks cannot see
  // by construction (calibrated with transcription; see README-calibration).
  const srcScope = coverage.sources.length
    ? coverage.sources.map(s => `${s.fmt} (${s.chapters}ch, all scanned)`).join(', ')
    : 'none';
  console.log('\n--- SCOPE OF THIS RUN ---');
  console.log(`karaoke_advances watched: reader chapter ${coverage.karaokeChapter == null ? '?' : coverage.karaokeChapter}` +
    `, ${coverage.karaokeWords} words on screen, audio near ${coverage.karaokeAudioSec == null ? '?' : coverage.karaokeAudioSec}s` +
    ' — ONE ~10s window in ONE chapter.');
  console.log(`switch_source scanned: ${srcScope} — near-empty INTERIOR chapters, every chapter of every source.`);
  console.log('CANNOT SEE (by construction):');
  console.log('  1. A UNIFORMLY shifted map. If every word time is off by the same amount the UI stays');
  console.log('     internally consistent, so these DOM checks pass. That class belongs to the timing probe.');
  console.log('  2. Timing/karaoke damage in chapters this run did not open. karaoke_advances samples ONE');
  console.log('     window; per-chapter sweeps cost ~4x. A green karaoke means THAT window advanced, not the whole book.');
  console.log('     (switch_source DOES cover all chapters, but only for the near-empty-render class.)');
  console.log('NOW COVERED (was a blind spot; added as its own journey):');
  console.log('  * The reader-does-NOT-follow-audio gap (PJ\'s stuck-"Cratchit") is caught by resume_flow, which');
  console.log('    RESUMES from a deep planted position (not a play-through — the bug lives at the transition).');
  console.log('    karaoke_advances alone cannot see it (it co-locates reader+audio by construction).');
  // ---- RULE 4 artifacts: every absorbed transient and every drive error, verbatim.
  console.log('\n--- HARNESS WAITS (RULE 4: bounded, recorded; the runner retried NOTHING) ---');
  if (!waits.length) console.log('  (none)');
  for (const w of waits) console.log(`  ${w.outcome === 'ready' ? 'ready  ' : 'TIMEOUT'}  ${w.label}  after ${w.attempts} attempt${w.attempts === 1 ? '' : 's'} / ${w.ms}ms`);
  console.log('--- DRIVE ERRORS (recorded, never swallowed; each fails its journey) ---');
  if (!driveErrors.length) console.log('  (none)');
  for (const d of driveErrors) console.log(`  ${d.journey} :: ${d.step} :: ${d.error}`);
  console.log(`\n${results.length - failed.length}/${results.length} journeys passed`);
  return failed.length ? 1 : 0;
}
