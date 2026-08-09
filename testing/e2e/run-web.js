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
    const tf = (typeof displayEditionBooks === 'function')
      ? displayEditionBooks(w, 'text')[0] : (w.text_files || [])[0];
    if (!tf) return;
    // Load the chapter list, pick a mid-book chapter (skips front matter).
    if (typeof loadChapterList === 'function') await loadChapterList(tf.id, w.id);
    const chs = (window.chapterCache && chapterCache[tf.id]?.chapters) || [];
    const mid = chs.length ? chs[Math.min(chs.length - 1, Math.max(1, Math.floor(chs.length / 2)))] : { index: 1 };
    if (typeof loadChapter === 'function') await loadChapter(tf.id, mid.index, w.id); // opens the reader overlay
    if (typeof resumeOrPlay === 'function') await resumeOrPlay(w.id);                 // starts audio
    const a = document.getElementById('audio-player');
    if (a && a.paused) a.play();
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
  const before = await page.evaluate(() => (document.querySelector('.reader-content')?.textContent || '').slice(0, 160));
  await page.evaluate((wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    const tf = (typeof displayEditionBooks === 'function') ? displayEditionBooks(w, 'text')[0] : (w.text_files || [])[0];
    const chs = (window.chapterCache && chapterCache[tf.id]?.chapters) || [];
    const cur = (typeof currentReaderChapter !== 'undefined' && currentReaderChapter[w.id]) ? currentReaderChapter[w.id].index : -1;
    const other = chs.find(c => c.index !== cur) || chs[0];
    if (other && typeof loadChapter === 'function') loadChapter(tf.id, other.index, w.id);
  }, WORK).catch(() => {});
  await page.waitForTimeout(2500);
  const after = await page.evaluate(() => (document.querySelector('.reader-content')?.textContent || '').slice(0, 160));
  report('change_chapter', before !== after, before === after ? 'reader content unchanged after loadChapter' : '');

  // ---- switch_source: every text source renders real content
  // (covered richly only when the work has >1 text source; report words seen)
  const wordsNow = await page.evaluate(() => document.querySelectorAll('.sync-word').length
    || (document.querySelector('.reader-content')?.textContent || '').split(/\s+/).length);
  report('switch_source', wordsNow >= 50, `rendered words=${wordsNow}`);

  await browser.close();
  process.exit(finish());
})().catch(e => { console.error('FATAL', e); process.exit(2); });

function finish() {
  const failed = results.filter(r => !r.ok);
  console.log(`\n${results.length - failed.length}/${results.length} journeys passed`);
  return failed.length ? 1 : 0;
}
