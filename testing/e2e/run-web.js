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
// What THIS run actually looked at — printed in the limits block so a green run
// is green about something specific, not "the whole book is fine".
const coverage = { karaokeChapter: null, karaokeWords: 0, karaokeAudioSec: null, sources: [] };
function report(id, ok, detail) {
  results.push({ id, ok, detail });
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${id}  ${detail || ''}`);
}
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
  const consoleErrors = [];
  page.on('pageerror', e => consoleErrors.push(String(e)));

  // ---- open_library
  await page.goto(BASE, { waitUntil: 'networkidle' });
  const cards = await page.locator('.work-card, [class*=card]').count();
  report('open_library', cards >= 1 && consoleErrors.length === 0,
    `cards=${cards} pageErrors=${consoleErrors.length}`);

  // ---- open_book (DETERMINISTIC: open the work under test by id, via the app's
  // own openWorkDetail, so the journey exercises a KNOWN work — not whichever
  // card happens to be first. server-web owns this navigation.)
  let opened = await page.evaluate((wid) => {
    if (typeof openWorkDetail !== 'function') return false;
    openWorkDetail(Number(wid));
    return true;
  }, WORK).catch(() => false);
  await page.waitForTimeout(1800);
  opened = opened && (await page.locator('text=/AUDIOBOOK|EBOOK/i').count()) > 0;
  report('open_book', opened, opened ? '' : 'openWorkDetail did not render the work');
  if (!opened) { await page.screenshot({ path: `${SHOTS}/open_book-FAIL.png` }); process.exit(finish()); }

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
  }, WORK).catch(() => {});
  await page.waitForTimeout(2200);
  await page.evaluate((wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    const sd = (typeof activeSyncData !== 'undefined' && activeSyncData) ? activeSyncData : null;
    if (sd && sd.length) {
      const idx = Math.min(sd.length - 1, Math.max(0, Math.floor(sd.length * 0.3)));
      const at = sd[idx].s;
      if (typeof seekToAbsoluteBookTime === 'function') seekToAbsoluteBookTime(w, at, w.title);
      else { const a = document.getElementById('audio-player'); if (a) a.currentTime = at; }
    }
    const a = document.getElementById('audio-player');
    if (a && a.paused) a.play().catch(() => {});
  }, WORK).catch(() => {});
  await page.waitForTimeout(1500);
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
  await page.evaluate(() => { const a = document.getElementById('audio-player'); if (a && !a.paused) a.pause(); });
  await page.waitForTimeout(400);
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
    await new Promise(r => setTimeout(r, 1500));
    const now = (typeof currentReaderChapter !== 'undefined' && currentReaderChapter[w.id]) ? currentReaderChapter[w.id].index : cur;
    return { ok: now === other.index && now !== cur, from: cur, to: now, wanted: other.index };
  }, WORK).catch((e) => ({ ok: false, why: String(e) }));
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
  await page.goto(BASE, { waitUntil: 'networkidle' });
  // PRECONDITION: plant a DEEP saved position, because the fixture's imported one
  // (1591.8s) is clobbered by any prior playback autosave — including this suite's
  // own earlier journeys. resume_flow owns its precondition so it is deterministic
  // regardless of run order. Plant AFTER the reload (no live player to re-clobber),
  // ~50% through the book (mid a later file), then resume via the real resumeOrPlay
  // with NO further reload between plant and resume.
  const planted = await page.evaluate(async (wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    const files = (w.audio_files || []);
    if (!files.length) return null;
    const total = files.reduce((s, f) => s + (f.duration_secs || 0), 0);
    const target = total * 0.5;
    let acc = 0, book = files[0], idx = 0, local = 0;
    for (let i = 0; i < files.length; i++) {
      const d = files[i].duration_secs || 0;
      if (acc + d >= target) { book = files[i]; idx = i; local = Math.round(target - acc); break; }
      acc += d;
    }
    await fetch(`/api/works/${w.id}/position`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ book_id: book.id, file_index: idx, position_secs: local }),
    }).catch(() => {});
    return { bookId: book.id, fileIdx: idx, local, globalApprox: Math.round(target) };
  }, WORK).catch(() => null);
  await page.evaluate((wid) => { if (typeof openWorkDetail === 'function') openWorkDetail(Number(wid)); }, WORK).catch(() => {});
  await page.waitForTimeout(1500);
  await page.evaluate((wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    if (w && typeof resumeOrPlay === 'function') resumeOrPlay(w.id); // resumes at the DEEP planted position
    const a = document.getElementById('audio-player');
    if (a && a.paused) a.play().catch(() => {});
  }, WORK).catch(() => {});
  await page.waitForTimeout(4000);
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
    let wordPairs = 0;
    try {
      const cov = await fetch(`/api/works/${w.id}/coverage`).then(r => r.json());
      wordPairs = (cov.pairs || []).filter(p => p.unit === 'word').length;
    } catch {}
    return { isEbookKaraoke, wordPairs };
  }, WORK).catch(() => ({ isEbookKaraoke: false, wordPairs: 0 }));
  // Reader followed iff the audio clock sits within the shown chapter's map extent.
  // Using the map EXTENT (not the highlighted word) avoids a false "harness" verdict
  // on a resume that hasn't reached its first word yet.
  const withinChapter = rs.mapMaxSec != null && rs.mapMinSec != null && rc != null
    && rc <= rs.mapMaxSec + 2.5 && rc >= rs.mapMinSec - 2.5;
  const notApplicable = ctx2.isEbookKaraoke && ctx2.wordPairs === 0; // synthetic map, nothing to follow
  const rFollows = notApplicable || (rs.wordCount >= 50 && withinChapter);
  const plantStr = planted ? `planted@~${planted.globalApprox}s(book ${planted.bookId} idx${planted.fileIdx}+${planted.local}s)` : 'plant FAILED';
  let rsig;
  if (notApplicable) rsig = `n/a: display ebook has NO word alignment (synthetic map) — reader-follow not certifiable on this work; give it a real alignment to test`;
  else if (rs.wordCount === 0) rsig = `harness: reader never opened (words=0) while clock=${rc} [${plantStr}] — fix the resume drive`;
  else if (!withinChapter) rsig = `stuck: clock=${rc} is PAST the shown chapter's map [${rs.mapMinSec == null ? '?' : rs.mapMinSec.toFixed(0)}..${rs.mapMaxSec == null ? '?' : rs.mapMaxSec.toFixed(0)}s] [${plantStr}] — reader did NOT follow audio (PJ's 'Cratchit')`;
  else rsig = `reader followed: clock=${rc} within map [${rs.mapMinSec.toFixed(0)}..${rs.mapMaxSec.toFixed(0)}s], ${rs.wordCount} words [${plantStr}]`;
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
  console.log(`\n${results.length - failed.length}/${results.length} journeys passed`);
  return failed.length ? 1 : 0;
}
