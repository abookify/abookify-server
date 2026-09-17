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
// Live/auth server support: a Bearer token the app pairs with and the runner's
// own API calls carry. On PJ's live server (auth on), use the dev token. Empty
// for the no-auth fixtures.
const AUTH_TOKEN = process.env.E2E_AUTH_TOKEN || '';
// Targeting a SPECIFIC work in a big library with duplicate titles (e.g. 3
// "A Christmas Carol"s on the live server): E2E_WORK_ID pins the API side to one
// work; E2E_SEARCH filters the library first; E2E_CARD_KEY is the node text
// tapped to OPEN the right card (a distinguishing badge like "11 audio"),
// defaulting to WORK_SUB.
const WORK_ID = process.env.E2E_WORK_ID || '';
const SEARCH = process.env.E2E_SEARCH || '';
const CARD_KEY = process.env.E2E_CARD_KEY || WORK_SUB;
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
const AUTH_HDR = AUTH_TOKEN ? `-H "Authorization: Bearer ${AUTH_TOKEN}"` : '';
function apiJson(pathAndQuery) {
  try { return JSON.parse(sh(`curl -s -m8 ${AUTH_HDR} "http://localhost:${PORT}${pathAndQuery}"`)); }
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
  // Out of disk: a nearly-full guest /data makes the app crash on launch
  // ("Failed to free … on /data") and can wedge the dump — presenting as a
  // crashing app or a red journey when the real cause is storage. Name it here.
  // Fix at the SOURCE: raise disk.dataPartition.size in the AVD's config.ini and
  // relaunch with -wipe-data (the image, not the host disk, is the ceiling).
  try {
    const dfLine = adb('shell df /data').trim().split('\n').pop();
    const availKb = +(dfLine.match(/\s(\d+)\s+\d+%/) || [])[1]; // Avail is the col before Use%
    if (availKb && availKb < 700000) {
      console.error(`INFRA(3) emulator OUT OF DISK — guest /data has only ${(availKb / 1024).toFixed(0)} MB free ` +
        `(<700 MB). The app crashes on launch / installs fail at this level; this is NOT an app failure. ` +
        `Raise disk.dataPartition.size in ~/.android/avd/<AVD>.avd/config.ini and relaunch with -wipe-data.`);
      process.exit(3);
    }
  } catch { /* df parse best-effort; don't block on a parse miss */ }
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

// ── Media-session state (the reliable playback truth) ────────────────────────
// `uiautomator dump` FAILS with "could not get idle state" on any screen with a
// live animation — the karaoke highlight + the mini-player waveform never let
// the window go idle. So the on-screen probe can only be read while PAUSED. The
// player's own state, however, is always available from `dumpsys media_session`
// (no idle needed, works mid-playback) — this is the source of truth for
// "is it playing" and "did the clock advance".
function mediaState() {
  let out = '';
  try { out = adb('shell dumpsys media_session'); } catch { return null; }
  // The app's media3 session line: state=PlaybackState {state=PLAYING(3), position=12345, ...}
  const m = out.match(/state=PlaybackState \{state=([A-Z]+)\((\d)\), position=(-?\d+), buffered position=(-?\d+), speed=([-0-9.]+)/);
  if (!m) return null;
  return { state: m[1], pos: +m[3] / 1000, buf: +m[4] / 1000, speed: +m[5] };
}
// Send a media transport key and confirm the player reached the target state.
// 126 = KEYCODE_MEDIA_PLAY, 127 = KEYCODE_MEDIA_PAUSE (explicit, not the 85
// toggle — a toggle races the current state).
async function mediaSet(wantPlaying, timeoutMs = 5000) {
  const deadline = Date.now() + timeoutMs;
  const want = wantPlaying ? 'PLAYING' : 'PAUSED';
  while (Date.now() < deadline) {
    const s = mediaState();
    if (s && s.state === want) return s;
    adb(`shell input keyevent ${wantPlaying ? 126 : 127}`);
    await sleep(800);
  }
  return mediaState();
}
const mediaPause = () => mediaSet(false);
const mediaPlay = () => mediaSet(true);

// ── The karaoke probe over LOGCAT (idle-proof) ───────────────────────────────
// The Reader/NowPlaying screens ALSO emit the E2E probe to logcat (console.log →
// tag ReactNativeJS). This is the only way to read karaoke state WHILE PLAYING:
// a uiautomator dump fails on the animating screen, and pausing to dump clears
// the reader's sync (it loads only while playingThisWork). logcatClear() before
// a play window, then logcatProbes() parses every probe emitted during it.
function logcatClear() { try { adb('logcat -c'); } catch {} }
function logcatProbes() {
  let out = '';
  try { out = adb('logcat -d'); } catch { return []; }
  const probes = [];
  for (const m of out.matchAll(/E2E(\{.*?\})/g)) {
    try { probes.push(JSON.parse(m[1])); } catch { /* skip a torn line */ }
  }
  return probes;
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
    const k = tag.match(/\bclass="([^"]*)"/);
    const [x1, y1, x2, y2] = [+b[1], +b[2], +b[3], +b[4]];
    out.push({
      text: decode(t ? t[1] : ''),
      desc: decode(d ? d[1] : ''),
      cls: k ? k[1] : '',
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
// EXACT accessibility-label match — for controls whose label is an ordinary
// phrase that can also occur in BOOK TEXT on screen. A substring search for
// "Next chapter" matched the karaoke paragraph "…The next chapter introduces…"
// (dump order puts the text pane before the transport bar) and tapped a WORD
// (a word-seek: probe mapS == pos to the decimal) instead of ⏭ (2026-09-17).
function findLabel(xml, label) {
  const l = label.toLowerCase();
  return nodes(xml).find((n) => n.desc.toLowerCase() === l || n.text.toLowerCase() === l);
}
function tapLabel(xml, label) {
  const n = findLabel(xml, label);
  if (!n) return false;
  tap(n.cx, n.cy);
  return true;
}

// Playback is live when the work's play circle has flipped to Pause, the work
// card reads "Playing", or a mini-player ("Now playing:") is on screen. NOT
// "Paused" (the work is loaded but stopped) — we require audio actually running.
function isPlaying(xml) {
  return /\bPlaying\b/i.test(xml) || /Now playing:/i.test(xml) || !!findNode(xml, 'Pause');
}

// Start playback robustly. The 'Play this book' circle tap is INTERMITTENT on
// the emulator: resumeWork() is async (it fetches saved position before the
// clock engages), a "Not checked against the audio" trust banner can shift the
// button's coordinates between the dump and the tap, and a single tap sometimes
// lands before the screen settled. So: re-dump for FRESH coordinates each try,
// tap the play circle by its accessibilityLabel, then wait for a real playing
// state; retry up to `tries` times. Returns true once playing.
async function startPlayback(tries = 4) {
  for (let i = 0; i < tries; i++) {
    const s = mediaState();
    if (s && s.state === 'PLAYING') return true;
    // The work screen (not yet playing) is static → dumpable. Tap the play
    // CIRCLE (label 'Play this book'), never the "Play book" text (no onPress).
    // Fresh node each try so a shifted layout can't stale the tap. Confirm via
    // the media session (the on-screen "Playing" text is unreliable — while
    // playing the work-screen dump barely reaches idle and captures a stale
    // frame; the player state never lies).
    const xml = dump();
    if (!tapText(xml, 'Play this book')) { await sleep(1500); continue; }
    const after = await mediaSet(true, 6000);
    if (after && after.state === 'PLAYING') return true;
  }
  const s = mediaState();
  return !!(s && s.state === 'PLAYING');
}

// Open the Reader (a probe screen) from the mini-player. The mini-player overlay
// is DROPPED from the accessibility tree while its waveform animates, but it
// reappears — with a labelled "Open reader" (📖) control — once PLAYBACK IS
// PAUSED. So: pause → dump → tap "Open reader". Returns true if it navigated
// (the paused Reader carries the E2E probe). Leaves playback PAUSED.
async function openReaderPaused() {
  // Retry the whole pause→dump→tap up to 3× BEFORE conceding INFRA. This drive
  // step was a ~50% coin-flip on the marginal headless emulator (2026-08-14):
  // one dump landed on the launcher HOME screen (the app had transiently lost
  // foreground — NOT a crash: empty crash buffer, no LMK kill) so "Open reader"
  // was absent and the run mis-conceded INFRA, which only passed because the
  // runner improvised its own re-run. Keeping the retry HERE means the harness
  // absorbs the flake itself and a genuine failure (app truly gone) still fails
  // honestly after 3 real attempts — a cold relaunch below has no mini-player,
  // so it can't manufacture a false green.
  for (let attempt = 0; attempt < 3; attempt++) {
    await mediaPause();
    let xml = dump(); // paused → window idle → dump succeeds
    if (parseProbe(xml) != null) return true; // already on a probe screen
    // Lost the app foreground (neither the probe nor ANY abookify node in the
    // tree — e.g. the launcher HOME screen)? Bring the app's EXISTING task back
    // to front (LAUNCHER intent resumes the task; it does not restart it or drop
    // nav state) and re-dump before deciding this attempt failed.
    if (!/com\.abookify/.test(xml)) {
      adb(`shell monkey -p ${PKG} -c android.intent.category.LAUNCHER 1`);
      await sleep(1500);
      xml = dump();
      if (parseProbe(xml) != null) return true;
    }
    if (tapText(xml, 'Open reader')) {
      const r = await waitFor((x) => parseProbe(x) != null, 6000);
      if (parseProbe(r) != null) return true;
    }
    await sleep(1200); // settle, then retry the pause/dump
  }
  return false;
}

// Reach the FULL PLAYER (NowPlaying) paused — the screen that carries the
// chapter TOC + the ⏭/⏮ controls + the E2E probe. Identified by its labeled
// "Chapters" control (the Reader also renders the probe, so the probe alone
// isn't proof). From any content screen the paused mini-player's "Now playing:"
// row opens it; from the Reader we back out first. Same 3× pause→dump→tap
// hardening as openReaderPaused (fire-and-forget taps on a slow host).
const onNowPlaying = (xml) => !!findLabel(xml, 'Chapters — jump to a chapter');
async function openNowPlayingPaused() {
  for (let attempt = 0; attempt < 4; attempt++) {
    await mediaPause();
    let xml = dump();
    if (onNowPlaying(xml)) return xml;
    if (!/com\.abookify/.test(xml)) {
      adb(`shell monkey -p ${PKG} -c android.intent.category.LAUNCHER 1`);
      await sleep(1500);
      xml = dump();
      if (onNowPlaying(xml)) return xml;
    }
    if (tapText(xml, 'Now playing:')) {
      xml = await waitFor(onNowPlaying, 8000);
      if (onNowPlaying(xml)) return xml;
    } else {
      keyBack(); // e.g. on the Reader (no mini-player row) → back to the work page
    }
    await sleep(1200);
  }
  return null;
}

// The work's chapter timeline the way the APP derives it (utils/chapters.ts):
// over the work's text sources, the one with the most chapters carrying a real
// start_sec wins; 'part' rows excluded. Book-global seconds.
function apiTimeline(workId) {
  const full = apiJson(`/api/works/${workId}`);
  if (!full) return { chapters: [], audio: [] };
  let best = []; let bestScore = -1;
  for (const tb of (full.text_files || [])) {
    const chs = (apiJson(`/api/books/${tb.id}/chapters`) || []).filter((c) => c.src !== 'part');
    const score = chs.filter((c) => (c.start_sec || 0) > 0).length;
    if (score > bestScore) {
      bestScore = score;
      best = chs.map((c) => ({ index: c.index, start_sec: c.start_sec || 0, end_sec: c.end_sec || 0, title: c.title || '' }));
    }
  }
  return { chapters: best, audio: full.audio_files || [] };
}
// Which FILE (index + local offset) a book-global second falls in — mirrors
// player.ts resolveBookSec (sidecar start_sec, else cumulative durations).
function fileOf(audio, bookSec) {
  const haveStart = audio.some((t) => typeof t.start_sec === 'number' && t.start_sec > 0);
  let cum = 0;
  for (let i = 0; i < audio.length; i++) {
    const s = haveStart ? (audio[i].start_sec || 0) : cum;
    const d = audio[i].duration_secs || 0;
    if ((bookSec >= s && bookSec < s + d) || i === audio.length - 1) return { index: i, local: bookSec - s, fileDur: d };
    cum += d;
  }
  return { index: 0, local: bookSec, fileDur: 0 };
}
// Scroll the open TOC sheet until a row containing `title` is on screen, then
// tap it. The sheet opens scrolled to the CURRENT chapter, so the target may be
// above or below: swipe to the top first, then page down. Returns the xml the
// tap was made on, or null.
async function tapTocRow(title) {
  // Geometry facts measured on the Pixel_7 AVD (2026-09-17): the sheet opens
  // scrolled to the CURRENT chapter; uiautomator dumps EVERY row, and a row
  // clipped by the sheet's ScrollView comes back with INVERTED bounds (y1 > y2)
  // — tapping those does nothing and leaves the sheet open. Fast flings don't
  // scroll this list; slow drags (~900ms) do, ~1:1. So: find the row; tap it
  // only when upright and inside the scroller; else drag slowly in the
  // direction its geometry indicates and re-dump. Verify the tap CLOSED the
  // sheet (jumpToChapter → setShowToc(false)) before calling it done.
  const W = 1080; // Pixel_7 AVD; bounds are in px
  const x = Math.round(W * 0.4);
  const drag = (fromY, toY) => adb(`shell input swipe ${x} ${fromY} ${x} ${toY} 900`);
  for (let step = 0; step < 10; step++) {
    const xml = dump();
    const ns = nodes(xml);
    const header = findNode(xml, 'Chapters (');
    if (!header) return null;
    const scroller = ns.find((n) => /ScrollView/.test(n.cls) && n.y1 >= header.y1) || { y1: header.y2, y2: 2300 };
    const row = ns.find((n) => n.y1 > header.y1 && /starts at/i.test(n.desc) && n.desc.toLowerCase().includes(title.toLowerCase()));
    if (!row) { drag(scroller.y2 - 120, scroller.y1 + 120); await sleep(1200); continue; } // not even laid out → page on
    const visible = row.y1 < row.y2 && row.y1 >= scroller.y1 - 2 && row.y2 <= scroller.y2 + 2;
    if (visible) {
      tap(row.cx, row.cy);
      const after = await waitFor((x2) => !findNode(x2, 'Chapters ('), 4000, 700);
      if (!findNode(after, 'Chapters (')) return xml;
      continue; // sheet still open → the tap missed; re-measure and retry
    }
    // Clipped: below the fold when its top sits at/after the scroller's bottom
    // region; above when its top is at the scroller's top (inverted bounds).
    const below = row.y1 >= scroller.y2 - 40 || (row.y1 > row.y2 && row.y1 > scroller.y1 + 40);
    if (below) drag(scroller.y2 - 120, scroller.y1 + 120); else drag(scroller.y1 + 120, scroller.y2 - 120);
    await sleep(1200);
  }
  // Give up, but never leave the sheet open over the transport controls.
  if (findNode(dump(), 'Chapters (')) tapLabel(dump(), 'Close chapters');
  return null;
}
// After a chapter jump: let the (re-anchored) stream play a few seconds, pause,
// and read where the APP says it is (probe pos/ch, book-global) + the media
// session's own clock. `settleMs` covers a reload over the host link.
async function landAfterJump(settleMs = 6000) {
  await sleep(settleMs);
  const ms = mediaState();
  await mediaPause();
  await sleep(600);
  const xml = dump();
  return { p: parseProbe(xml), ms, xml };
}

// ── The mobile "DOM contract": the E2E{...} probe + the mini-player clock ─────
// Latest E2E{...} line in the dump → parsed JSON, or null.
// uiautomator quotes an attribute VALUE with single quotes when the value itself
// contains double quotes — and our probe JSON does (`{"widx":...}`). So the node
// arrives as text='E2E{"widx":...}' (single-quoted attr, LITERAL inner quotes),
// NOT text="E2E{&quot;widx&quot;:...}" (double-quoted attr, escaped quotes). We
// match BOTH forms — this exact mismatch made a rendering probe read as absent.
function parseProbe(xml) {
  const re = /text=(?:"(E2E\{.*?\})"|'(E2E\{.*?\})')/g;
  let last = null, m;
  while ((m = re.exec(xml)) !== null) last = m[1] || m[2];
  if (last == null) return null;
  try { return JSON.parse(decode(last).slice(3)); } catch { return null; }
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

  // Attempt 1 — deep link (one command). auth_token is the dev/bypass token on an
  // auth server (E2E_AUTH_TOKEN), else a throwaway a no-auth server ignores.
  const deep = `abookify://pair?url=${encodeURIComponent(HOST_URL)}&auth_token=${AUTH_TOKEN || 'e2e'}`;
  // Double-quote the whole device command so the `&` in the URL survives BOTH
  // the local shell AND the device shell (an unquoted & backgrounds the command
  // and drops the package arg → "com.abookify.app not found").
  adb(`shell "am start -a android.intent.action.VIEW -d '${deep}' ${PKG}"`);
  // 45s, not 25s: the deep-link pair itself is fast, but the LIBRARY RENDER that
  // libraryUp waits for is slow on a loaded emulator (many works + active
  // generation jobs + the software GPU) and was overrunning 25s — the connect had
  // actually SUCCEEDED (the library appeared seconds later) but the check gave up
  // first and mis-reported connected=false. waitFor polls, so this only spends the
  // extra time on a genuinely slow render, never on a fast one.
  let xml = await waitFor(libraryUp, 45000);
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
  // Big library / duplicate titles: filter with the search box first so the
  // target card is on screen (e.g. 3 "A Christmas Carol"s on the live server).
  if (connected && SEARCH) {
    // Type the filter, then VERIFY it surfaced the target card, retrying if not.
    // Fire-and-forget `tap`+`typeText` has no wait-for-focus, so on a slow
    // software-GPU emulator the type can outrun the field focusing (keyboard/
    // focus animation) and be silently dropped — the list never filters and the
    // card reads as "not found" (an observed flake, 2026-08-15). Clear any prior
    // partial before each retry so a re-type can't append onto a half-typed query.
    for (let attempt = 0; attempt < 3 && !findNode(xml, CARD_KEY); attempt++) {
      const clear = findNode(xml, 'Clear search');
      if (clear) { tap(clear.cx, clear.cy); await sleep(300); xml = dump(); }
      const field = findNode(xml, 'Search title');
      if (!field) break;
      tap(field.cx, field.cy); await sleep(700);
      typeText(SEARCH); await sleep(1300);
      xml = dump();
    }
  }
  const cardFound = !!findNode(xml, CARD_KEY);
  report('open_library', connected && cardFound,
    `connected=${connected} card(${CARD_KEY})=${cardFound}`);
  if (!(connected && cardFound)) { shot('open_library'); process.exit(finish()); }

  // ---- open_book: tap the target work card (by CARD_KEY — a distinguishing
  // badge like "11 audio" when the title alone is ambiguous), assert the work
  // page rendered. After a search the soft keyboard is still UP: a tap that
  // lands while it's animating/covering can hit a KEY instead (observed
  // 2026-09-17: the query became "Selfishq" and no card was tapped). Dismiss it
  // (Back only closes the keyboard here), re-dump for fresh coordinates, and
  // retry the tap — a dropped tap must not read as a broken work page.
  const workUp = (x) => /Play book|Playing|Paused/i.test(x) && !!findNode(x, WORK_SUB);
  if (SEARCH) { keyBack(); await sleep(700); xml = dump(); }
  for (let t = 0; t < 3 && !workUp(xml); t++) {
    if (!findNode(xml, CARD_KEY)) { xml = dump(); }
    tapText(xml, CARD_KEY);
    xml = await waitFor(workUp, 12000);
  }
  const onWork = workUp(xml);
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
    const w = WORK_ID ? works.find((x) => String(x.id) === String(WORK_ID))
      : works.find((x) => (x.title || '').includes(WORK_SUB));
    const full = w ? apiJson(`/api/works/${w.id}`) : null;
    if (!full) {
      report('cross_surface_counts', false, `could not read API canonical for ${WORK_ID ? `work ${WORK_ID}` : `"${WORK_SUB}"`}`);
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

  // ---- Reset the saved playback position to the START before measuring.
  // resumeWork() resumes to the server-saved position; a prior run (or a real
  // user) can leave it at the END of the book, where the clock is parked and
  // CANNOT advance — every timing assert would then false-fail. Seeking to 0
  // gives a known low starting point (chapter 1) with room to advance. This is
  // legitimate test setup, not masking a bug: the end-state is created by
  // earlier playback, never a defect. (Sampling chapter 1 is also why the
  // book-global-vs-chapter-relative map scale can't bite here — both ~0 — a
  // limitation finish() already discloses.)
  // A book's FIRST audio file is often a short spoken title card ("A CHRISTMAS
  // CAROL") that carries NO word-sync, so karaoke can't advance there. On a
  // multi-file work, seek into the SECOND file (the first narrated chapter) a
  // bit past its start; on a single-file work, 20s in. E2E_START_FILE /
  // E2E_START_SEC override.
  {
    const list = apiJson('/api/works') || [];
    const works = Array.isArray(list) ? list : (list.works || []);
    const w = WORK_ID ? works.find((x) => String(x.id) === String(WORK_ID))
      : works.find((x) => (x.title || '').includes(WORK_SUB));
    const af = (w && w.audio_files) || [];
    const idx = process.env.E2E_START_FILE != null ? +process.env.E2E_START_FILE : (af.length > 1 ? 1 : 0);
    const secs = process.env.E2E_START_SEC != null ? +process.env.E2E_START_SEC : (af.length > 1 ? 90 : 20);
    const bookId = (af[idx] || af[0] || {}).id;
    if (w && bookId != null) {
      try {
        sh(`curl -s -m8 -X POST ${AUTH_HDR} "http://localhost:${PORT}/api/works/${w.id}/position" ` +
           `-H 'Content-Type: application/json' ` +
           `-d '{"work_id":${w.id},"book_id":${bookId},"file_index":${idx},"position_secs":${secs}}'`);
        console.log(`seeded position: work ${w.id} file_index=${idx} (book ${bookId}) @ ${secs}s`);
      } catch { /* best-effort; a nonzero start just narrows the advance window */ }
    }
  }

  // ---- play_and_hear: start playback and confirm the PLAYER CLOCK advances
  // with wall time. The clock is read from `dumpsys media_session` (the real
  // player state) — NOT a UI dump, which fails to reach idle while the karaoke
  // and waveform animate. startPlayback() taps 'Play this book' (retrying on the
  // intermittent tap) and confirms PLAYING via the media session.
  const started = await startPlayback();
  if (!started) {
    // Playback never engaged — the actual "F1 won't play" failure PJ hit on the
    // messy Carol. Red it here and stop (nothing downstream can run).
    report('play_and_hear', false, 'playback never started (Play tap did not reach a PLAYING media session after retries)');
    shot('play_and_hear');
    report('karaoke_advances', false, 'skipped — playback never started');
    shot('karaoke_advances');
    skip('change_chapter', 'playback never started');
    skip('switch_source', 'playback never started');
    skip('export_import_populated', 'playback never started');
    skip('download_offline_play', 'playback never started');
    skip('signout_signin', 'playback never started');
    process.exit(finish());
  }
  const t0 = Date.now(); const m1 = mediaState();
  await sleep(10000);
  const m2 = mediaState(); const wall = (Date.now() - t0) / 1000;
  const delta = m1 && m2 ? m2.pos - m1.pos : null;
  const heard = delta != null && delta >= 3 && Math.abs(delta - wall) <= 3 && m2.state === 'PLAYING';
  report('play_and_hear', heard,
    `player ${m1 ? m1.pos.toFixed(1) : '?'}s -> ${m2 ? m2.pos.toFixed(1) : '?'}s ` +
    `(+${delta == null ? '?' : delta.toFixed(1)}s) over ${wall.toFixed(1)}s wall, state=${m2 ? m2.state : '?'}`);
  if (!heard) shot('play_and_hear');

  // ---- karaoke_advances: A1-A5, the same karaoke contract as calibrate-karaoke.js,
  // read over LOGCAT while PLAYING. The probe only renders on the Reader/
  // NowPlaying, which never go idle while animating (uiautomator dump fails), and
  // pausing to dump clears the reader's sync. So: reach the Reader (pause just to
  // tap "Open reader"), then RESUME and read the probe stream the app emits to
  // logcat over a 11s play window. p1/p2 are the first/last probes → A3 (widx
  // advanced) and A5 (clock advanced) measure REAL motion during real playback.
  // E2E_SKIP_KARAOKE=1: skip the reader/karaoke legs (a chapter-navigation-only
  // calibration, e.g. against a build whose reader has no logcat probe).
  const skipKaraoke = process.env.E2E_SKIP_KARAOKE === '1';
  if (skipKaraoke) {
    skip('karaoke_advances', 'E2E_SKIP_KARAOKE=1'); skip('resume_reader_follows', 'E2E_SKIP_KARAOKE=1');
  } else {
  if (!(await openReaderPaused())) {
    console.error('INFRA(3) could not reach the Reader (no "Open reader" control while paused) — ' +
      'cannot place the probe on screen.');
    shot('probe-absent'); process.exit(3);
  }
  logcatClear();
  const tPlay = Date.now();
  await mediaPlay(); // resume — the Reader now streams E2E probes to logcat
  await sleep(11000);
  // Read the probe stream BEFORE pausing: pausing clears the reader's sync
  // (isPlayingThisWork → false), which emits a trailing widx:-1/words:0 frame
  // that must not be mistaken for the end-of-window sample.
  const probesRaw = logcatProbes();
  const playWall = (Date.now() - tPlay) / 1000;
  await mediaPause();
  if (!probesRaw.length) {
    // No probe in logcat while the Reader played → the installed APK lacks the
    // probe (not an EXPO_PUBLIC_E2E=1 build). Refuse a false karaoke app-fail.
    console.error('INFRA(3) no E2E probe in logcat while playing the Reader — ' +
      'not an EXPO_PUBLIC_E2E=1 build (needs .env.local + a full `./gradlew clean`).');
    shot('probe-absent'); process.exit(3);
  }
  // Keep only real karaoke frames (sync loaded, a word active): drops the
  // pre-sync-load frames right after resume and any transient reset on a chapter
  // auto-advance. If NONE are valid, karaoke genuinely never engaged → A1 reds.
  const probes = probesRaw.filter((p) => typeof p.widx === 'number' && p.widx >= 0 && (p.words || 0) > 0);
  const p1 = probes[0] || {}; const p2 = probes[probes.length - 1] || {};
  const clockAdv = (typeof p1.pos === 'number' && typeof p2.pos === 'number') ? p2.pos - p1.pos : null;
  const A1 = (p2.words || 0) >= 50;
  const A2 = typeof p2.widx === 'number' && p2.widx >= 0;
  const A3 = A2 && typeof p1.widx === 'number' && p1.widx >= 0 && p2.widx > p1.widx;
  const A4 = typeof p2.mapS === 'number' && p2.mapS >= 0 && typeof p2.pos === 'number'
    && Math.abs(p2.mapS - p2.pos) <= 2.5;
  // Assert helper so we can re-sample after a catch-up nav (below) without dup.
  const assess = (raw, wall) => {
    const ps = raw.filter((p) => typeof p.widx === 'number' && p.widx >= 0 && (p.words || 0) > 0);
    const a = ps[0] || {}; const b = ps[ps.length - 1] || {};
    const adv = (typeof a.pos === 'number' && typeof b.pos === 'number') ? b.pos - a.pos : null;
    const A1 = (b.words || 0) >= 50;
    const A2 = typeof b.widx === 'number' && b.widx >= 0;
    const A3 = A2 && typeof a.widx === 'number' && a.widx >= 0 && b.widx > a.widx;
    const A4 = typeof b.mapS === 'number' && b.mapS >= 0 && typeof b.pos === 'number' && Math.abs(b.mapS - b.pos) <= 2.5;
    const A5 = adv != null && adv >= 3 && Math.abs(adv - wall) <= 3;
    return { ps, a, b, adv, A1, A2, A3, A4, A5, ok: A1 && A2 && A3 && A4 && A5, wall,
      line: `[${ps.length}/${raw.length} valid] A1 words=${b.words} A2 widx=${b.widx} A3 ${a.widx}->${b.widx} `
        + `A4 |map ${b.mapS == null ? 'null' : (+b.mapS).toFixed(1)} - pos ${b.pos == null ? 'null' : (+b.pos).toFixed(1)}| `
        + `A5 +${adv == null ? '?' : adv.toFixed(1)}s/${wall.toFixed(1)}s` };
  };
  let R = assess(probesRaw, playWall);
  // THE RESUME→EBOOK-KARAOKE BUG (server-web owned, dispatched): after a resume
  // the reader can stay pinned to front-matter chapter 0 on the ebook-word-
  // karaoke follow path — never advancing to the chapter the audio plays. This
  // path is the SHIPPING config (the bundled TTS-only sample + every Kokoro/
  // GPU-less book), so we test it as its OWN journey (`resume_reader_follows`),
  // AND still certify steady-state karaoke by navigating onto the audio's chapter.
  // Signature: widx frozen AND the reader map sits far BEHIND the audio position.
  const readerStuckOnResume = !R.ok
    && typeof R.a.widx === 'number' && R.a.widx === R.b.widx
    && typeof R.b.mapS === 'number' && typeof R.b.pos === 'number' && (R.b.pos - R.b.mapS) > 10;

  // ---- karaoke_advances: STEADY-STATE word-karaoke on the ebook path. If the
  // reader opened stuck (resume bug), navigate it onto the audio's chapter (the
  // reader "Next" control seeks reader+audio together) and re-sample — so this
  // journey certifies "word karaoke actually advances on the ebook path" on the
  // TTS-only shipping config, independent of the resume-follow bug.
  if (readerStuckOnResume) {
    await mediaPause(); await sleep(500);
    const x = dump();
    if (tapText(x, 'Next:') || tapText(x, 'Next chapter')) {
      await sleep(2500);
      logcatClear();
      const t2 = Date.now();
      await mediaPlay();
      await sleep(11000);
      const raw2 = logcatProbes();
      const w2 = (Date.now() - t2) / 1000;
      await mediaPause();
      if (raw2.length) R = assess(raw2, w2);
    }
  }
  report('karaoke_advances', R.ok,
    `${R.line}${readerStuckOnResume ? ' [steady-state after catch-up nav — reader had opened stuck on ch0]' : ''}`);
  if (!R.ok) shot('karaoke_advances');

  // ---- resume_reader_follows: on a RESUME into the ebook-word-karaoke path, the
  // reader must follow to the chapter the audio is in — NOT stay pinned on
  // front-matter ch0. EXPECTED-RED today (known server-web bug); flips to green
  // when they land the fix. Surfaced as its own journey so the bug is never
  // calibrated away by the steady-state certification above.
  report('resume_reader_follows', !readerStuckOnResume,
    readerStuckOnResume
      ? 'reader stuck on front-matter ch0 while audio played on (map far behind pos) — KNOWN server-web resume→ebook-karaoke bug (dispatched); EXPECTED-RED until fixed'
      : 'reader followed the audio onto its chapter after resume');
  if (readerStuckOnResume) shot('resume_reader_follows');
  } // end !skipKaraoke

  // ---- change_chapter / switch_source / export_import_populated — later.
  skip('change_chapter', 'next increment');
  skip('switch_source', 'next increment');
  skip('export_import_populated', 'next increment');

  // ---- download_offline_play (mobile-owned): download to the device, then play
  // with the radio OFF. MUST fail loudly if the download stalls (Resume / error)
  // — that's PJ's download bug.
  // E2E_SKIP_DOWNLOAD=1 skips this leg (a 100+MB fetch + airplane-mode toggle
  // that destabilizes adb on this emulator) for a fast KARAOKE-only calibration.
  let airplaneOn = false;
  if (process.env.E2E_SKIP_DOWNLOAD === '1') {
    skip('download_offline_play', 'E2E_SKIP_DOWNLOAD=1 (karaoke-only calibration)');
  } else try {
    // Back out of the reader to the work page where the download control lives.
    // keyBack is fire-and-forget; on a saturated host a press can be dropped, so
    // retry until a work-page download control appears (driver-hardening pattern —
    // see testing/e2e/driver-flakiness-diagnosis.md).
    const dlControl = (x) => /Add to device|On device|Resume \(|Update available/i.test(x);
    for (let b = 0; b < 3 && !dlControl(xml); b++) {
      keyBack();
      xml = await waitFor(dlControl, 8000);
    }
    if (!dlControl(xml)) throw new Error('could not reach the work-page download control (back-nav did not land after 3 tries)');
    if (/On device/i.test(xml)) {
      // Already downloaded from a prior run — a present on-device copy still
      // satisfies the offline-playback assertion, so proceed without re-fetching.
    } else if (/Resume \(/i.test(xml)) {
      // Entered on a killed/resumable download — that IS PJ's download bug; fail loudly.
      throw new Error('download control is in the RESUMABLE (stalled) state on entry — PJ\'s download stall');
    } else {
      // Tap "Add to device" and VERIFY the download STARTED (the badge flips off
      // "Add to device" to a "N% · …" / "Unpacking…" state), retrying the tap — a
      // dropped tap on a saturated host would silently never start, then time out
      // at 180s and read as a stall it isn't.
      let started = false;
      for (let t = 0; t < 3 && !started; t++) {
        if (!tapText(xml, 'Add to device')) throw new Error('no "Add to device" control on the work page');
        xml = await waitFor((x) => !findNode(x, 'Add to device'), 8000);
        started = !findNode(xml, 'Add to device');
      }
      if (!started) throw new Error('tapped "Add to device" but the download never started (badge unchanged after 3 taps)');
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

    // Go offline and confirm local playback still advances — measured from the
    // media session (same idle-proof approach as play_and_hear).
    adb('shell cmd connectivity airplane-mode enable'); airplaneOn = true;
    await sleep(2500);
    const playingOffline = await startPlayback();
    if (!playingOffline) throw new Error('offline: playback did not start (no PLAYING media session)');
    const ot0 = Date.now(); const om1 = mediaState();
    await sleep(10000);
    const om2 = mediaState(); const owall = (Date.now() - ot0) / 1000;
    const odelta = om1 && om2 ? om2.pos - om1.pos : null;
    const offlineOk = odelta != null && odelta >= 3 && Math.abs(odelta - owall) <= 3 && om2.state === 'PLAYING';
    report('download_offline_play', offlineOk,
      `downloaded; offline player ${om1 ? om1.pos.toFixed(1) : '?'}s -> ${om2 ? om2.pos.toFixed(1) : '?'}s ` +
      `(+${odelta == null ? '?' : odelta.toFixed(1)}s) over ${owall.toFixed(1)}s`);
    if (!offlineOk) shot('download_offline_play');
  } catch (e) {
    report('download_offline_play', false, e.message);
    shot('download_offline_play');
  } finally {
    // ALWAYS restore connectivity, even on an early throw.
    if (airplaneOn) { try { adb('shell cmd connectivity airplane-mode disable'); } catch {} }
  }

  // ---- chapter_seek / next_chapter / prev_chapter (mobile-owned, board #21 —
  // PJ's Selfish Gene: nine EQUAL-SIZE files, chapters straddle the splits).
  // On a work WITH a chapter timeline: (a) tapping a TOC row must land the audio
  // at that chapter's book-global start — into the RIGHT FILE at the RIGHT
  // OFFSET (the silent-seek-break class); (b) ⏭ must move by CHAPTER, not by
  // file (the jump that stranded PJ); (c) ⏮ restarts the chapter. Read from the
  // NowPlaying probe while PAUSED (`pos` = the book-global second the app
  // renders, `ch` = chapter index) plus the media session's sub-stream clock —
  // a streamed MP3 chapter jump is a ?t=-anchored RELOAD, so that clock restarts
  // near 0: proof the stream was re-anchored, not seeked in place in a stale
  // file. Tolerance ±(settle + 8)s on pos: it PLAYS between the jump and the
  // pause. SKIPPED (not failed) when the work has no timeline.
  // E2E_SKIP_CHAPTERS=1 skips; E2E_CHAPTER_N=<1-based row> pins the TOC target.
  if (process.env.E2E_SKIP_CHAPTERS === '1') {
    skip('chapter_seek', 'E2E_SKIP_CHAPTERS=1'); skip('next_chapter', 'E2E_SKIP_CHAPTERS=1'); skip('prev_chapter', 'E2E_SKIP_CHAPTERS=1');
  } else try {
    const list = apiJson('/api/works') || [];
    const works = Array.isArray(list) ? list : (list.works || []);
    const w = WORK_ID ? works.find((x) => String(x.id) === String(WORK_ID))
      : works.find((x) => (x.title || '').includes(WORK_SUB));
    const { chapters, audio } = w ? apiTimeline(w.id) : { chapters: [], audio: [] };
    const timed = chapters.filter((c) => c.start_sec > 0);
    if (!w || timed.length < 2) {
      skip('chapter_seek', 'work has no chapter timeline (fewer than 2 timed chapters)');
      skip('next_chapter', 'no chapter timeline'); skip('prev_chapter', 'no chapter timeline');
    } else {
      const npXml = await openNowPlayingPaused();
      if (!npXml) throw new Error('could not reach the full player (no labeled "Chapters" control after 4 tries)');
      // The landing assertions read the probe; without it this is an INFRA
      // condition (a non-EXPO_PUBLIC_E2E build), never an app red.
      if (parseProbe(npXml) == null) {
        console.error('INFRA(3) no E2E probe on the full player — not an EXPO_PUBLIC_E2E=1 build.');
        shot('probe-absent'); process.exit(3);
      }
      // Where is the playhead now (book-global)? From the probe on the paused player.
      const p0 = parseProbe(npXml) || {};
      const curFile = typeof p0.pos === 'number' ? fileOf(audio, p0.pos).index : -1;
      // Target: a REAL chapter (≥60s long — an aligned title page can be a
      // 0.6s sliver) that starts MID-FILE (≥60s from either edge of its file),
      // preferring one AHEAD of the playhead in a LATER file (PJ's actual move:
      // a cross-file landing forward), else any mid-file one, else the 2nd.
      const dur = (c) => { const nx = timed.filter((o) => o.start_sec > c.start_sec).sort((a, b) => a.start_sec - b.start_sec)[0]; return (c.end_sec > c.start_sec ? c.end_sec : (nx ? nx.start_sec : c.start_sec + 1e9)) - c.start_sec; };
      const real = (c) => dur(c) >= 60;
      const midFile = (c) => { const f = fileOf(audio, c.start_sec); return audio.length > 1 && f.local >= 60 && f.fileDur - f.local >= 60; };
      const pos0 = typeof p0.pos === 'number' ? p0.pos : 0;
      let target = null;
      // E2E_CHAPTER=<title substring> pins the target by name (e.g. "Memes").
      if (process.env.E2E_CHAPTER) target = timed.find((c) => (c.title || '').toLowerCase().includes(process.env.E2E_CHAPTER.toLowerCase())) || null;
      if (!target && process.env.E2E_CHAPTER_N) target = chapters[+process.env.E2E_CHAPTER_N - 1] || null;
      if (!target) target = timed.find((c) => real(c) && midFile(c) && c.start_sec > pos0 && fileOf(audio, c.start_sec).index > curFile)
        || timed.find((c) => real(c) && midFile(c)) || timed.find(real) || timed[1];
      const tf = fileOf(audio, target.start_sec);
      const title = (target.title || '').trim() || `Chapter ${chapters.indexOf(target) + 1}`;
      const where = (c) => { const f = fileOf(audio, c.start_sec); return `${c.start_sec.toFixed(1)}s = file ${f.index} @ ${f.local.toFixed(0)}s`; };
      const SETTLE = 6000;
      // Landing = the ANCHOR the stream was re-started at: probe pos (book-
      // global, read after the pause) minus the media session's sub-stream
      // clock (read just before it). That is exact to ~3s regardless of how
      // long the audio played between the tap and the pause (a verified row
      // tap can take 20s of drags/re-dumps; the seek itself is instant). The
      // raw pos is still sanity-checked (never BEFORE the start, and within a
      // generous play window) for the case the media clock is unreadable.
      const landed = (r, c) => {
        const pos = r.p && typeof r.p.pos === 'number' ? r.p.pos : null;
        const clock = r.ms && typeof r.ms.pos === 'number' ? r.ms.pos : null;
        const anchor = pos != null && clock != null ? pos - clock : null;
        const okAnchor = anchor != null ? Math.abs(anchor - c.start_sec) <= 4 : true;
        const okPos = pos != null && pos >= c.start_sec - 2 && pos <= c.start_sec + 40;
        const okCh = r.p && r.p.ch === c.index;
        return { okPos, okCh, ok: okAnchor && okPos && okCh,
          line: `anchor=${anchor == null ? '?' : anchor.toFixed(1)} (Δ${anchor == null ? '?' : (anchor - c.start_sec >= 0 ? '+' : '') + (anchor - c.start_sec).toFixed(1)}s from start; pos ${pos == null ? '?' : pos.toFixed(1)} after ${clock == null ? '?' : clock.toFixed(1)}s played) ch=${r.p ? r.p.ch : '?'} (want ${c.index})` };
      };

      // (a) chapter_seek: open the TOC (the labeled control), tap the row, land.
      if (!tapLabel(npXml, 'Chapters — jump to a chapter')) throw new Error('no "Chapters" control on the full player');
      const sheet = await waitFor((x) => !!findNode(x, 'Chapters ('), 5000);
      if (!findNode(sheet, 'Chapters (')) throw new Error('the chapter sheet did not open');
      // ---- toc_named: the sheet must list NAMED chapters (not "N file-sized
      // lumps"): header count vs the API timeline, and the visible rows are
      // titles rather than "Part/Track/File N".
      {
        const hdr = findNode(sheet, 'Chapters (');
        const shown = +((hdr.text.match(/Chapters \((\d+)\)/) || [])[1] || 0);
        const rows = nodes(sheet).filter((n) => n.y1 > hdr.y2 && /starts at/i.test(n.desc)).map((n) => n.desc.replace(/^Now playing: /, '').replace(/, starts at.*$/, ''));
        const lumps = rows.filter((t) => /^(part|track|file|disc)\s*\d+/i.test(t) || /unabridged \d/i.test(t));
        const ok = shown >= Math.min(timed.length, 10) && rows.length > 0 && lumps.length === 0;
        report('toc_named', ok, `sheet "Chapters (${shown})" vs API ${timed.length} timed; visible rows: ${rows.slice(0, 4).map((t) => `"${t}"`).join(', ')}${rows.length > 4 ? ', …' : ''}${lumps.length ? ` — ${lumps.length} file-lump row(s)` : ''}`);
        if (!ok) shot('toc_named');
      }
      if (!(await tapTocRow(title))) throw new Error(`TOC row "${title}" not found in the sheet after scrolling`);
      const r1 = await landAfterJump(SETTLE);
      const L1 = landed(r1, target);
      report('chapter_seek', L1.ok, `tapped "${title}" (${where(target)}) → ${L1.line}`);
      if (!L1.ok) shot('chapter_seek');

      // (b) next_chapter: ⏭ must be CHAPTER-aware. The control's label says which
      // it is: "Next chapter" on a chaptered work; "Next track" = the file-skip
      // bug (PJ's). Then it must land on the NEXT chapter's start.
      const after = timed.filter((c) => c.start_sec > target.start_sec + 0.5).sort((a, b) => a.start_sec - b.start_sec)[0];
      if (!after) {
        skip('next_chapter', `"${title}" is the last chapter`); skip('prev_chapter', 'no next chapter to come back from');
      } else {
        let xml = r1.xml;
        const isTrack = !!findLabel(xml, 'Next track') && !findLabel(xml, 'Next chapter');
        if (isTrack) {
          report('next_chapter', false, '⏭ is labeled "Next track" on a chaptered work — it skips a FILE, not a chapter (PJ\'s Selfish Gene bug)');
          shot('next_chapter'); skip('prev_chapter', '⏭ not chapter-aware');
        } else {
          if (!tapLabel(xml, 'Next chapter')) throw new Error('no "Next chapter" control on the full player');
          const r2 = await landAfterJump(SETTLE);
          const L2 = landed(r2, after);
          report('next_chapter', L2.ok, `⏭ from "${title}" → expected "${(after.title || '').trim()}" (${where(after)}) → ${L2.line}`);
          if (!L2.ok) shot('next_chapter');

          // (c) prev_chapter: a few seconds into `after`, ⏮ RESTARTS it (>3s in).
          xml = r2.xml;
          if (!tapLabel(xml, 'Previous chapter')) throw new Error('no "Previous chapter" control on the full player');
          const r3 = await landAfterJump(SETTLE);
          const L3 = landed(r3, after);
          report('prev_chapter', L3.ok, `⏮ ${r2.p && typeof r2.p.pos === 'number' ? (r2.p.pos - after.start_sec).toFixed(1) : '?'}s into "${(after.title || '').trim()}" → restart it (${where(after)}) → ${L3.line}`);
          if (!L3.ok) shot('prev_chapter');
        }
      }
    }
  } catch (e) {
    report('chapter_seek', false, e.message); shot('chapter_seek');
    skip('next_chapter', 'chapter_seek errored'); skip('prev_chapter', 'chapter_seek errored');
  }

  // ---- signout_signin — later.
  skip('signout_signin', 'next increment');

  process.exit(finish());
})().catch((e) => { console.error('FATAL', e && e.stack ? e.stack : e); shot('fatal'); process.exit(2); });
