#!/usr/bin/env node
// One-off diagnostic: what does the reader ACTUALLY show after a DEEP resume?
// Distinguishes real stuck-Cratchit (reader on title chapter, highlight frozen,
// audio deep) from a runner artifact. Not part of the suite.
const args = {};
for (let i = 2; i < process.argv.length; i += 2) args[process.argv[i].replace(/^--/, '')] = process.argv[i + 1];
const BASE = args.base;
const WORK = args.work;
const PW = args.pw;
const { chromium } = require(PW);
(async () => {
  const browser = await chromium.launch({ executablePath: '/usr/bin/chromium', args: ['--no-sandbox', '--autoplay-policy=no-user-gesture-required'] });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  if (args.cookie) await ctx.addCookies([{ name: 'abookify_session', value: args.cookie, domain: new URL(BASE).hostname, path: '/' }]);
  const page = await ctx.newPage();
  await page.goto(BASE, { waitUntil: 'networkidle' });
  // plant deep
  const planted = await page.evaluate(async (wid) => {
    const w = (allWorks || []).find(x => x.id === Number(wid));
    const files = (w.audio_files || []);
    const total = files.reduce((s, f) => s + (f.duration_secs || 0), 0);
    const target = total * 0.5;
    let acc = 0, book = files[0], idx = 0, local = 0;
    for (let i = 0; i < files.length; i++) { const d = files[i].duration_secs || 0; if (acc + d >= target) { book = files[i]; idx = i; local = Math.round(target - acc); break; } acc += d; }
    await fetch(`/api/works/${w.id}/position`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ book_id: book.id, file_index: idx, position_secs: local }) });
    return { bookId: book.id, fileIdx: idx, local, globalApprox: Math.round(target) };
  }, WORK);
  await page.evaluate((wid) => { openWorkDetail(Number(wid)); }, WORK);
  await page.waitForTimeout(1500);
  await page.evaluate((wid) => { const w = allWorks.find(x => x.id === Number(wid)); resumeOrPlay(w.id); const a = document.getElementById('audio-player'); if (a && a.paused) a.play().catch(() => {}); }, WORK);
  await page.waitForTimeout(6000);
  const snap = await page.evaluate((wid) => {
    const player = document.getElementById('audio-player');
    const words = document.querySelectorAll('.sync-word');
    const read = [...document.querySelectorAll('.sync-word.read')].map(e => +e.dataset.widx).filter(n => !isNaN(n));
    const map = (typeof activeSyncData !== 'undefined') ? activeSyncData : null;
    const w = (allWorks || []).find(x => x.id === Number(wid));
    const rc = (typeof currentReaderChapter !== 'undefined' && currentReaderChapter[w.id]) ? currentReaderChapter[w.id] : null;
    const readerText = (document.querySelector('.reader-content')?.innerText || '').slice(0, 120).replace(/\s+/g, ' ');
    return {
      audioSrc: (player.src || '').split('/').slice(-2).join('/'),
      audioCurrentTime: Math.round(player.currentTime),
      audioPaused: player.paused,
      playerTimeText: (document.querySelector('.player-time')?.textContent || '').trim(),
      readerChapterIndex: rc ? rc.index : null,
      currentReaderIsEbookKaraoke: (typeof currentReaderIsEbookKaraoke !== 'undefined') ? currentReaderIsEbookKaraoke : 'undef',
      currentSyncChapterIdx: (typeof currentSyncChapterIdx !== 'undefined') ? currentSyncChapterIdx : 'undef',
      syncWordCount: words.length,
      mapLen: map ? map.length : 0,
      mapMin: map && map.length ? map[0].s : null,
      mapMax: map && map.length ? map[map.length - 1].s : null,
      readWordsCount: read.length,
      maxReadWidx: read.length ? Math.max(...read) : null,
      readerHead: readerText,
    };
  }, WORK);
  console.log('PLANTED:', JSON.stringify(planted));
  console.log('AFTER DEEP RESUME (6s play):', JSON.stringify(snap, null, 2));
  await page.screenshot({ path: `${args.shots || '/tmp/e2e-shots'}/probe-resume.png`, fullPage: false });
  await browser.close();
})().catch(e => { console.error('FATAL', e); process.exit(2); });
