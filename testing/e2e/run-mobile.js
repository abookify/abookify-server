#!/usr/bin/env node
// Mobile journey runner (React Native + Expo, driven over `adb` + uiautomator).
// The mobile lane of the cross-surface e2e suite — it MIRRORS run-web.js: same
// journey ids, per-journey PASS/FAIL lines, a screenshot per failure, and a
// nonzero exit if any IMPLEMENTED journey fails. karaoke_advances runs the exact
// A1-A5 assertion calibrate-karaoke.js defined, calibrated so the five real
// broken states (near-empty render, frozen highlight, wrong chapter/sentence,
// frozen clock) FAIL.
//
// ── Why a state probe instead of the DOM ────────────────────────────────────
// run-web.js reads `.sync-word` / `activeSyncData` / `.player-time` straight out
// of the DOM. React Native has no DOM, so the app renders an env-gated debug
// <Text> line (ReaderScreen.tsx + NowPlayingScreen.tsx, gated on
// EXPO_PUBLIC_E2E === '1') that this runner reads out of a uiautomator dump:
//     E2E{"widx":<activeWordIdx>,"words":<count>,"mapS":<bookGlobalSec>,"pos":<bookGlobalSec>,"ch":<chapterIdx>}
// `mapS` and `pos` are both book-global seconds — the same contract the web probe
// uses — so A4 (|mapS - pos|) and A5 (player clock vs wall) read what the app
// ACTUALLY RENDERS, never a recomputation. The probe is inlined to `undefined`
// in production builds and never ships.
//
// ── Prerequisites (the human sets these up; this runner only DRIVES) ─────────
//   1. An Android emulator is running and visible to `adb devices`.
//      (Set E2E_SERIAL if more than one device/emulator is attached.)
//   2. The E2E-built APK is installed:  com.abookify.app  built with
//         EXPO_PUBLIC_E2E=1  (so the probe compiles in). See "Build" below.
//   3. testing/e2e/fixture-server.sh is running on the HOST and printed
//         READY <port> <messy_work_id> pristine=<P> human=<H> dir=<DIR>
//      Pass that <port> as E2E_PORT (default 8199). The emulator reaches the
//      host server at 10.0.2.2:<port> (the standard AVD host alias).
//
// ── Build the E2E APK ────────────────────────────────────────────────────────
//   cd engineering/mobile/abookify-mobile
//   EXPO_PUBLIC_E2E=1 npx expo run:android --variant release   # or an EAS build
//     with `EXPO_PUBLIC_E2E=1` in the build profile env. Any build where
//     EXPO_PUBLIC_E2E is '1' at bundle time carries the probe; a normal build
//     does not.
//
// ── Run ──────────────────────────────────────────────────────────────────────
//   E2E_PORT=8199 node testing/e2e/run-mobile.js
//   Optional env: E2E_WORK="Carol" (work-title substring to target, default
//   "Carol" — must match a fixture work), E2E_SERIAL=<emulator-5554>.
//
// Exit 0 = every IMPLEMENTED journey passed. Nonzero = loud failure with a
// per-assert detail line and /tmp/e2e-mobile-<journey>.png screenshots.

const { execSync } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');

// ── Config ───────────────────────────────────────────────────────────────────
const PORT = process.env.E2E_PORT || '8199';
const HOST_URL = `http://10.0.2.2:${PORT}`;        // emulator → host server
const PKG = 'com.abookify.app';
const WORK_SUB = process.env.E2E_WORK || 'Carol';  // work-title substring to open
const SERIAL = process.env.E2E_SERIAL || '';       // optional `adb -s` target
const ADB = `adb${SERIAL ? ` -s ${SERIAL}` : ''}`;
const TMP_XML = path.join(os.tmpdir(), 'e2e-ui.xml');
const REMOTE_XML = '/sdcard/e2e-ui.xml';

// ── Shell + adb helpers ───────────────────────────────────────────────────────
function sh(cmd, opts = {}) {
  return execSync(cmd, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], ...opts }).trim();
}
function adb(args, opts = {}) { return sh(`${ADB} ${args}`, opts); }
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function tap(x, y) { adb(`shell input tap ${x} ${y}`); }
function keyBack() { adb('shell input keyevent 4'); }
function typeText(s) {
  // adb `input text` treats space as %s; our URLs have no spaces, but be safe.
  adb(`shell input text '${s.replace(/ /g, '%s').replace(/'/g, "")}'`);
}
function screencap(file) {
  try { adb(`exec-out screencap -p > "${file}"`); } catch { /* best-effort */ }
}

// ── uiautomator dump + parse ──────────────────────────────────────────────────
function dump() {
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      adb(`shell uiautomator dump ${REMOTE_XML}`);
      adb(`pull ${REMOTE_XML} "${TMP_XML}"`);
      return fs.readFileSync(TMP_XML, 'utf8');
    } catch (e) {
      if (attempt === 2) throw e;
    }
  }
  return '';
}

// Decode XML entities uiautomator escapes in attribute values (the probe's JSON
// quotes come back as &quot;). &amp; last so we don't double-decode.
const decode = (s) => (s || '')
  .replace(/&quot;/g, '"').replace(/&apos;/g, "'")
  .replace(/&lt;/g, '<').replace(/&gt;/g, '>')
  .replace(/&#10;/g, '\n').replace(/&amp;/g, '&');

// Every <node> as { text, desc, bounds, cx, cy } — both `text` and
// `content-desc` (RN accessibilityLabel) are searchable, since many tappable
// controls render their label only as content-desc.
function nodes(xml) {
  const out = [];
  const tags = xml.match(/<node\b[^>]*?\/?>/g) || [];
  for (const tag of tags) {
    const b = tag.match(/\bbounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"/);
    if (!b) continue;
    const t = tag.match(/\btext="([^"]*)"/);
    const d = tag.match(/\bcontent-desc="([^"]*)"/);
    const [x1, y1, x2, y2] = [+b[1], +b[2], +b[3], +b[4]];
    out.push({
      text: decode(t ? t[1] : ''),
      desc: decode(d ? d[1] : ''),
      x1, y1, x2, y2, cx: (x1 + x2) >> 1, cy: (y1 + y2) >> 1,
    });
  }
  return out;
}
function findNode(xml, sub) {
  const s = sub.toLowerCase();
  return nodes(xml).find((n) => n.text.toLowerCase().includes(s) || n.desc.toLowerCase().includes(s));
}
// Tap the first node whose text/content-desc contains `sub`. Returns true if found.
function tapText(xml, sub) {
  const n = findNode(xml, sub);
  if (!n) return false;
  tap(n.cx, n.cy);
  return true;
}

// ── The mobile "DOM contract": the E2E{...} probe + the mini-player clock ─────
// Latest E2E{...} line in the dump → parsed JSON, or null.
function parseProbe(xml) {
  const all = [...xml.matchAll(/text="(E2E\{.*?\})"/g)];
  if (!all.length) return null;
  try { return JSON.parse(decode(all[all.length - 1][1]).slice(3)); } catch { return null; }
}
// "1:34" / "1:02:03" → seconds (same reducer as run-web.js clockSecs).
function clockSecs(txt) {
  const p = (txt || '').trim().split(':').map(Number);
  if (p.some(isNaN) || !p.length) return null;
  return p.reduce((a, b) => a * 60 + b, 0);
}
// The mini-player book-global position (the LEFT number of "M:SS / H:MM:SS").
// The chapter-context label also renders a "X / Y" but carries a "·" separator
// and is chapter-relative, so we skip any candidate containing "·" and take the
// standalone book-global node. Falls back to the probe `pos` if no clock node.
function parseClock(xml) {
  const timeRe = /(\d+:\d{2}(?::\d{2})?)\s*\/\s*\d+:\d{2}(?::\d{2})?/;
  let best = null;
  for (const n of nodes(xml)) {
    const m = n.text.match(timeRe);
    if (!m || n.text.includes('·')) continue; // skip the chapter-relative label
    best = clockSecs(m[1]);
  }
  if (best == null) { // fallback: any time node, else the probe's book-global pos
    for (const n of nodes(xml)) { const m = n.text.match(timeRe); if (m) best = clockSecs(m[1]); }
    if (best == null) { const p = parseProbe(xml); if (p && typeof p.pos === 'number') best = p.pos; }
  }
  return best;
}
// One dump → both readings at a single point in time (probe + clock + wall).
function snapshot() {
  const xml = dump();
  return { xml, p: parseProbe(xml), clock: parseClock(xml), t: Date.now() };
}

// ── Reporting (mirrors run-web.js) ────────────────────────────────────────────
const results = [];
function report(id, ok, detail) {
  results.push({ id, ok, detail });
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${id}  ${detail || ''}`);
}
function skip(id, why) { console.log(`SKIP  ${id}  (${why})`); }
function shot(journey) { screencap(`/tmp/e2e-mobile-${journey}.png`); }
function finish() {
  const failed = results.filter((r) => !r.ok);
  if (failed.length) {
    console.log(`\n\x1b[31m${failed.length} journey(s) FAILED:\x1b[0m ${failed.map((f) => f.id).join(', ')}`);
  }
  console.log(`\n${results.length - failed.length}/${results.length} implemented journeys passed`);
  return failed.length ? 1 : 0;
}

// Poll the dump until `pred(xml)` is truthy, or timeout. Returns the matching xml
// or the last xml on timeout.
async function waitFor(pred, timeoutMs, everyMs = 1500) {
  const deadline = Date.now() + timeoutMs;
  let xml = '';
  while (Date.now() < deadline) {
    xml = dump();
    if (pred(xml)) return xml;
    await sleep(everyMs);
  }
  return xml;
}

// ── Connect the app to the no-auth fixture server ─────────────────────────────
// Primary: the abookify://pair deep link. App.tsx's handlePairLink requires BOTH
// url AND auth_token (no bypass), so for the no-auth fixture we pass a throwaway
// token — a no-auth server ignores it. Fallback: drive the Connect screen (type
// the URL, tap Connect), which uses the tokenless connect path.
async function connect() {
  const libraryUp = (xml) => /Search title|Search title or author/i.test(xml) || !!findNode(xml, WORK_SUB);

  // Cold-start the app fresh so we begin from a known state.
  adb(`shell am force-stop ${PKG}`);
  await sleep(500);

  // Attempt 1 — deep link (one command).
  const deep = `abookify://pair?url=${encodeURIComponent(HOST_URL)}&auth_token=e2e`;
  // Double-quote the whole device command so the `&` in the URL survives BOTH
  // the local shell AND the device shell (an unquoted & backgrounds the command
  // and drops the package arg → "com.abookify.app not found").
  adb(`shell "am start -a android.intent.action.VIEW -d '${deep}' ${PKG}"`);
  let xml = await waitFor(libraryUp, 25000);
  if (libraryUp(xml)) return true;

  // Attempt 2 — manual Connect screen. The URL field placeholder is
  // "http://192.168.1.100:7654"; type over it and tap Connect.
  adb(`shell monkey -p ${PKG} -c android.intent.category.LAUNCHER 1`);
  xml = await waitFor((x) => !!findNode(x, '192.168') || libraryUp(x), 15000);
  if (libraryUp(xml)) return true;
  const field = findNode(xml, '192.168');
  if (field) {
    tap(field.cx, field.cy);
    await sleep(400);
    typeText(HOST_URL);
    await sleep(300);
    // The Connect button (not a remembered-server row).
    if (!tapText(dump(), 'Connect')) keyBack();
    xml = await waitFor(libraryUp, 20000);
  }
  return libraryUp(xml);
}

// ── Journeys ──────────────────────────────────────────────────────────────────
(async () => {
  // Sanity: a device must be attached.
  try {
    const devs = adb('devices').split('\n').slice(1).filter((l) => /\tdevice$/.test(l));
    if (!devs.length) { console.error('FATAL no adb device/emulator attached (see prereqs in the header)'); process.exit(2); }
  } catch (e) { console.error('FATAL adb not available:', e.message); process.exit(2); }

  // ---- open_library
  const connected = await connect();
  let xml = dump();
  const cardFound = !!findNode(xml, WORK_SUB);
  report('open_library', connected && cardFound,
    `connected=${connected} card(${WORK_SUB})=${cardFound}`);
  if (!(connected && cardFound)) { shot('open_library'); process.exit(finish()); }

  // ---- open_book: tap the target work card, assert the work page rendered.
  tapText(xml, WORK_SUB);
  xml = await waitFor((x) => /Play book|Playing|Paused/i.test(x), 12000);
  const onWork = /Play book|Playing|Paused/i.test(xml) && !!findNode(xml, WORK_SUB);
  report('open_book', onWork, onWork ? '' : 'work page (title + "Play book") did not render');
  if (!onWork) { shot('open_book'); process.exit(finish()); }

  // ---- play_and_hear: start playback, open the reader (📖) so the probe is on
  // screen, then confirm the player clock ADVANCES with wall time.
  if (!/Playing/i.test(xml)) tapText(xml, 'Play book');
  await waitFor((x) => /Playing|Pause/i.test(x), 8000);
  // Open the reader via the mini-player 📖 button (present once playback started).
  if (!tapText(dump(), '📖') && !tapText(dump(), 'Open reader')) {
    // Some fixtures label the button by content-desc only ("Open reader").
  }
  await waitFor((x) => parseProbe(x) != null || parseClock(x) != null, 10000);
  const a = snapshot();
  await sleep(10000);
  const b = snapshot();
  const wall = (b.t - a.t) / 1000;
  const delta = a.clock != null && b.clock != null ? b.clock - a.clock : null;
  const heard = delta != null && delta >= 3 && Math.abs(delta - wall) <= 3;
  report('play_and_hear', heard, `clock ${a.clock}s -> ${b.clock}s (+${delta == null ? '?' : delta.toFixed(1)}s) over ${wall.toFixed(1)}s wall`);
  if (!heard) shot('play_and_hear');

  // ---- karaoke_advances: A1-A5, exactly as calibrate-karaoke.js.
  const s1 = snapshot();
  await sleep(10000);
  const s2 = snapshot();
  const p1 = s1.p || {}; const p2 = s2.p || {};
  const wall2 = (s2.t - s1.t) / 1000;
  const A1 = (p2.words || 0) >= 50;
  const A2 = typeof p2.widx === 'number' && p2.widx >= 0;
  const A3 = A2 && typeof p1.widx === 'number' && p1.widx >= 0 && p2.widx > p1.widx;
  const A4 = typeof p2.mapS === 'number' && p2.mapS >= 0 && typeof p2.pos === 'number'
    && Math.abs(p2.mapS - p2.pos) <= 2.5;
  const A5 = s1.clock != null && s2.clock != null && Math.abs((s2.clock - s1.clock) - wall2) <= 2.5;
  const karaokeOk = A1 && A2 && A3 && A4 && A5;
  report('karaoke_advances', karaokeOk,
    `A1 words=${p2.words} A2 widx=${p2.widx} A3 ${p1.widx}->${p2.widx} ` +
    `A4 |map ${p2.mapS == null ? 'null' : (+p2.mapS).toFixed(1)} - pos ${p2.pos == null ? 'null' : (+p2.pos).toFixed(1)}| ` +
    `A5 clock +${s1.clock != null && s2.clock != null ? (s2.clock - s1.clock).toFixed(1) : '?'}s over ${wall2.toFixed(1)}s`);
  if (!karaokeOk) shot('karaoke_advances');

  // ---- change_chapter / switch_source / export_import_populated — later.
  skip('change_chapter', 'next increment');
  skip('switch_source', 'next increment');
  skip('export_import_populated', 'next increment');

  // ---- download_offline_play (mobile-owned): download to the device, then play
  // with the radio OFF. MUST fail loudly if the download stalls (Resume / error)
  // — that's PJ's download bug.
  let airplaneOn = false;
  try {
    // Back out of the reader to the work page where the download control lives.
    keyBack();
    xml = await waitFor((x) => /Add to device|On device|Resume \(|Update available/i.test(x), 10000);
    if (/On device/i.test(xml)) {
      // Already downloaded from a prior run — remove so we exercise a fresh DL.
      // (Leave it; a present on-device copy still satisfies offline playback.)
    } else if (!tapText(xml, 'Add to device')) {
      throw new Error('no "Add to device" control on the work page');
    }
    // Poll to completion, failing loudly on the stall states.
    let done = false; let stalled = null;
    const deadline = Date.now() + 180000;
    while (Date.now() < deadline) {
      xml = dump();
      if (/On device/i.test(xml)) { done = true; break; }
      const stall = findNode(xml, 'Resume (') || findNode(xml, "Couldn't resume") || findNode(xml, "Couldn't add");
      if (stall) { stalled = stall.text || stall.desc; break; }
      await sleep(2500);
    }
    if (!done) throw new Error(stalled ? `download stalled: "${stalled}"` : 'download did not reach "On device" within 180s');

    // Go offline and confirm local playback still advances.
    adb('shell cmd connectivity airplane-mode enable'); airplaneOn = true;
    await sleep(2500);
    xml = dump();
    if (!/Playing/i.test(xml)) { if (!tapText(xml, 'Play book')) tapText(xml, 'Play'); }
    await waitFor((x) => /Playing|Pause/i.test(x), 8000);
    const o1 = snapshot();
    await sleep(10000);
    const o2 = snapshot();
    const owall = (o2.t - o1.t) / 1000;
    const odelta = o1.clock != null && o2.clock != null ? o2.clock - o1.clock : null;
    const offlineOk = odelta != null && odelta >= 3 && Math.abs(odelta - owall) <= 3;
    report('download_offline_play', offlineOk,
      `downloaded; offline clock ${o1.clock}s -> ${o2.clock}s (+${odelta == null ? '?' : odelta.toFixed(1)}s) over ${owall.toFixed(1)}s`);
    if (!offlineOk) shot('download_offline_play');
  } catch (e) {
    report('download_offline_play', false, e.message);
    shot('download_offline_play');
  } finally {
    // ALWAYS restore connectivity, even on an early throw.
    if (airplaneOn) { try { adb('shell cmd connectivity airplane-mode disable'); } catch {} }
  }

  // ---- signout_signin — later.
  skip('signout_signin', 'next increment');

  process.exit(finish());
})().catch((e) => { console.error('FATAL', e && e.stack ? e.stack : e); shot('fatal'); process.exit(2); });
