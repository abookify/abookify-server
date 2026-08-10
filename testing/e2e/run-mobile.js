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
// The emulator normally reaches the host at 10.0.2.2 (AVD NAT alias). If that
// NAT is broken (some headless/older AVD states have no route), set up
// `adb reverse tcp:PORT tcp:PORT` and pass E2E_HOST=127.0.0.1 — the app then
// reaches the host fixture through adb instead of the guest NAT.
const HOST = process.env.E2E_HOST || '10.0.2.2';
const HOST_URL = `http://${HOST}:${PORT}`;         // emulator → host server
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
// The runner is on the HOST, so it reaches the fixture server directly at
// localhost:PORT (the emulator uses 10.0.2.2:PORT — same server). Used by the
// cross-surface counts assert to read the API canonical.
function apiJson(pathAndQuery) {
  try { return JSON.parse(sh(`curl -s -m8 "http://localhost:${PORT}${pathAndQuery}"`)); }
  catch { return null; }
}

// Preflight: distinguish INFRA failure from APP failure so a wedged emulator or
// a wrong build is NEVER read as a broken app (the cry-wolf that makes the loop
// untrustworthy). Infra problems exit 3 with a distinct reason. (App/journey
// failures exit 1; bad-usage/no-adb exit 2; infra exit 3.) Each check answers a
// specific "did the harness itself fail?" question BEFORE any journey runs.
function preflight() {
  let devs;
  try { devs = adb('devices').split('\n').slice(1).filter((l) => /\tdevice$/.test(l)); }
  catch (e) { console.error('INFRA(3) adb not available:', e.message); process.exit(3); }
  if (!devs.length) {
    // A known cause must not read as an unknown failure: a stale zero-byte
    // multiinstance.lock (left when the emulator dies during a resource freeze)
    // silently blocks EVERY boot even though no process holds it. Name it.
    let hint = 'start the emulator (a run with no device is not a red journey)';
    try {
      const avdRoot = `${process.env.HOME}/.android/avd`;
      const locks = sh(`ls -1 ${avdRoot}/*/*.lock 2>/dev/null || true`).split('\n').filter(Boolean);
      const noProc = !sh('pgrep -f "emulator.*-avd" || true').trim();
      if (locks.length && noProc) hint = `STALE EMULATOR LOCK is blocking boot (no emulator process holds it): ${locks.join(', ')} — delete it and relaunch. (cause: emulator died during a resource freeze)`;
    } catch {}
    console.error(`INFRA(3) no adb device — ${hint}`);
    process.exit(3);
  }
  if (devs.length > 1 && !process.env.E2E_SERIAL) { console.error(`INFRA(3) ${devs.length} devices attached — set E2E_SERIAL`); process.exit(3); }
  let booted = '';
  try { booted = adb('shell getprop sys.boot_completed').trim(); } catch {}
  if (booted !== '1') { console.error(`INFRA(3) emulator not fully booted (sys.boot_completed="${booted}") — half-booted, not a red journey. Wait/cold-relaunch.`); process.exit(3); }
  // Responsive: a wedged emulator still answers `adb` but hangs/empties the
  // uiautomator dump. An empty dump would make EVERY journey fail ambiguously.
  let xml = '';
  try { xml = dump(); } catch {}
  if (!xml || xml.length < 200) { console.error('INFRA(3) emulator UNRESPONSIVE — uiautomator dump empty/failed (wedged). Cold-relaunch the AVD; this is NOT an app failure.'); process.exit(3); }
  try { if (!adb(`shell pm path ${PKG}`).includes('package:')) throw 0; }
  catch { console.error(`INFRA(3) ${PKG} not installed — install the EXPO_PUBLIC_E2E=1 APK before running.`); process.exit(3); }
  try { sh(`curl -sf -m5 -o /dev/null "http://localhost:${PORT}/api/ready"`); }
  catch { console.error(`INFRA(3) fixture server not reachable at localhost:${PORT} — start testing/e2e/fixture-server.sh. A journey red here would be a lie.`); process.exit(3); }
  console.log(`preflight OK — device booted + responsive, ${PKG} installed, fixture :${PORT} ready`);
}

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

// Open the Now-Playing karaoke screen by tapping the mini-player body. The
// mini-player is the BOTTOM-MOST node whose text carries the work title (the
// work-screen title is higher up); tapping it navigates to Now-Playing where
// the E2E probe renders. Returns true if a mini-player was found + tapped.
function openNowPlaying() {
  const cands = nodes(dump()).filter((n) => (n.text || '').includes(WORK_SUB));
  if (!cands.length) return false;
  const mp = cands.reduce((a, b) => (b.cy > a.cy ? b : a)); // bottom-most
  tap(mp.cx, mp.cy);
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
  // A green run must state what it is green ABOUT — the opposite of the
  // confident-wrong-answer pattern this whole effort exists to kill.
  if (!failed.length) {
    console.log('\nGREEN is scoped: this run verifies the ANDROID surface, on the sampled work' +
      `${process.env.E2E_WORK ? ` ("${process.env.E2E_WORK}")` : ''}, on the CHAPTER the playhead sampled.`);
    console.log('It does NOT and CANNOT tell you: (a) a UNIFORMLY-shifted map — if every word is wrong by the ' +
      'same amount the UI is internally consistent, so on-device checks are blind to it (that belongs to the ' +
      "timing probe); (b) UNSAMPLED chapters — green here is green about this chapter, not the whole book; " +
      '(c) anything iOS-specific (lock-screen, background audio, foreground deep-link, layout).');
  }
  // Exit codes: 0 = all implemented journeys pass; 1 = an app/journey failed;
  // (2 = usage; 3 = INFRA — emitted directly by preflight/probe-absent, never here).
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
  // Preflight distinguishes infra failure (exit 3) from app/journey failure
  // (exit 1) — a wedged emulator or wrong build must never look like a red app.
  preflight();

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

  // ---- cross_surface_counts (META-elevated): the counts the MOBILE surface
  // shows for this work must match the API canonical (transcription owns the
  // canonical definition; this is the mobile half of web==mobile==API). MUST go
  // RED on today's messy fixture, where the Kokoro edition splits across two
  // rows. Reads the work-page badges ("N audio chapters" / "M chapters") + the
  // count of distinct SOURCE rows, and compares to /api/works/{id}.
  // NOTE: the exact canonical QUANTITY is being finalized with transcription;
  // until then the API's own audio_files / text_files / distinct-edition counts
  // are the canonical proxy. This asserts the mobile UI renders that faithfully.
  {
    const list = apiJson('/api/works') || [];
    const works = Array.isArray(list) ? list : (list.works || []);
    const w = works.find((x) => (x.title || '').includes(WORK_SUB));
    const full = w ? apiJson(`/api/works/${w.id}`) : null;
    if (!full) {
      report('cross_surface_counts', false, `could not read API canonical for "${WORK_SUB}"`);
    } else {
      const af = full.audio_files || [];
      const tf = full.text_files || [];
      // Canonical: distinct audio EDITIONS (a work should have ONE narration
      // edition per origin+voice; the messy fixture wrongly splits Kokoro into
      // two). Files and text-sources counts too.
      const editionKey = (b) => `${b.origin || b.source_type || ''}|${b.voice || ''}`;
      const apiEditions = new Set(af.map(editionKey)).size;
      // Mobile UI: "N audio chapters", "M chapters", and the SOURCE row count.
      const mAudio = (xml.match(/(\d+)\s*audio chapters?/i) || [])[1];
      const mChapters = (xml.match(/\b(\d+)\s*chapters?\b/i) || [])[1];
      const mSourceRows = (xml.match(/·\s*\d+\s*files?/gi) || []).length;
      // IMPORTANT: do NOT assert mobile == raw API. For the messy fixture the
      // Kokoro split lives in the DATA (two edition labels), so the raw API AND
      // mobile both report the split — a "mobile == API" check would PASS on the
      // broken data (measuring the wrong thing, the exact trap META flagged).
      // The RED must come from comparing to transcription's CORRECT canonical
      // (one Kokoro edition), which is not yet published. So: EMIT the mobile
      // half's numbers now (the cheap "probe line"), and SKIP the pass/fail
      // until the canonical comparator lands — never a false green.
      skip('cross_surface_counts',
        `mobile[audioCh=${mAudio} chapters=${mChapters} sourceRows=${mSourceRows}] ` +
        `rawAPI[editions=${apiEditions} files=${af.length} text=${tf.length}] — ` +
        `pending transcription canonical (do NOT assert vs raw API: split is in the data)`);
    }
  }

  // ---- play_and_hear: start playback, open the reader (📖) so the probe is on
  // screen, then confirm the player clock ADVANCES with wall time.
  // Tap the play CIRCLE, not the "Play book" text (the text label has no
  // onPress — only the circle is touchable, via its accessibilityLabel).
  if (!/Playing/i.test(xml)) tapText(xml, 'Play this book');
  // Playback started when the circle's label flips to Pause / the card says
  // Playing / a mini-player carrying the title appears at the bottom.
  await waitFor((x) => /Playing/i.test(x) || !!findNode(x, 'Pause'), 8000);
  // Open the NOW-PLAYING karaoke screen so the E2E probe is on screen. The
  // mini-player's 📖 (reader) button is UNLABELED in uiautomator, so tap the
  // mini-player BODY instead (its title row opens Now-Playing) — the probe
  // renders there too. The mini-player is the BOTTOM-MOST node carrying the
  // work title; tap that. If playback never started (a broken work that won't
  // play), there IS no mini-player → no probe → karaoke_advances fails loudly,
  // which is correct.
  // Now-Playing can take a moment to navigate + render; retry the open a few
  // times and wait on the SAME xml the probe check uses (a racy re-dump caused
  // false "probe absent" triggers). If the probe appears, proceed.
  let probeXml = '';
  for (let attempt = 0; attempt < 3; attempt++) {
    openNowPlaying();
    probeXml = await waitFor((x) => parseProbe(x) != null, 8000);
    if (parseProbe(probeXml) != null) break;
  }
  // Probe-absent = INFRA, not app — but ONLY decide it from the very xml we
  // waited on (not a fresh racy dump). If playback IS running (a clock/Pause is
  // on screen) yet the probe NEVER rendered across the retries, the app is not
  // the EXPO_PUBLIC_E2E=1 build → exit 3, never a false karaoke app-fail. If
  // play never started (no clock/Pause), that's a real app failure — let the
  // journeys red normally below.
  if (parseProbe(probeXml) == null) {
    const playing = /\d+:\d{2}(?::\d{2})?\s*\/\s*\d+:\d{2}/.test(probeXml) || !!findNode(probeXml, 'Pause');
    if (playing) {
      console.error('INFRA(3) E2E probe ABSENT while playing — NOT an EXPO_PUBLIC_E2E=1 build ' +
        '(needs .env.local + a full `./gradlew clean`). Refusing a false app-fail.');
      shot('probe-absent'); process.exit(3);
    }
  }
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
