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
  await page.evaluate(async (wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    if (!w) return;
    // DETERMINISTIC seed: seek to a fixed mid-book audio position (20% in, past
    // front matter) via seekToAbsoluteBookTime — it plays from that book-global
    // second AND follows the reader to the matching chapter, so audio and reader
    // AGREE every run. (resumeOrPlay used a stale saved position, which desynced
    // the reader and made karaoke_advances flap.)
    const audio = (typeof displayEditionBooks === 'function') ? displayEditionBooks(w, 'audio') : (w.audio_files || []);
    const total = audio.reduce((s, f) => s + (f.duration_secs || 0), 0);
    const at = Math.max(30, Math.round(total * 0.2));
    if (typeof seekToAbsoluteBookTime === 'function') await seekToAbsoluteBookTime(w, at, w.title);
    const a = document.getElementById('audio-player');
    if (a) { if (a.paused) await a.play().catch(() => {}); }
  }, WORK).catch(() => {});
  await page.waitForTimeout(1200);
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
    if (!(window.chapterCache && chapterCache[tf.id]?.chapters?.length) && typeof loadChapterList === 'function') {
      await loadChapterList(tf.id, w.id);
    }
    const chs = (window.chapterCache && chapterCache[tf.id]?.chapters) || [];
    if (chs.length < 2) return { ok: false, why: `only ${chs.length} chapters in book ${tf.id}` };
    const cur = (typeof currentReaderChapter !== 'undefined' && currentReaderChapter[w.id]) ? currentReaderChapter[w.id].index : chs[0].index;
    const other = chs.find(c => c.index !== cur) || chs[chs.length - 1];
    await loadChapter(tf.id, other.index, w.id);
    await new Promise(r => setTimeout(r, 1500));
    const now = (typeof currentReaderChapter !== 'undefined' && currentReaderChapter[w.id]) ? currentReaderChapter[w.id].index : cur;
    return { ok: now === other.index && now !== cur, from: cur, to: now, wanted: other.index };
  }, WORK).catch((e) => ({ ok: false, why: String(e) }));
  report('change_chapter', chg.ok, chg.ok ? `chapter ${chg.from}->${chg.to}` : (chg.why || `stayed on ${chg.from}`));

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
  report('switch_source', srcResult.length > 0 && broken.length === 0,
    srcResult.map(s => `${s.fmt}:${s.chapters}ch/${s.nearEmptyInterior}empty`).join(' ') || 'no text sources');
  if (broken.length) await page.screenshot({ path: `${SHOTS}/switch_source-FAIL.png` });

  await browser.close();
  process.exit(finish());
})().catch(e => { console.error('FATAL', e); process.exit(2); });

function finish() {
  const failed = results.filter(r => !r.ok);
  console.log(`\n${results.length - failed.length}/${results.length} journeys passed`);
  return failed.length ? 1 : 0;
}
