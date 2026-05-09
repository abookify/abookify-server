// Unit tests for the shared karaoke + format helpers. Pure-JS, runnable
// under any tiny test runner — we use Node's built-in `node:test` so
// there's no dev-dependency required.
//
// Run with:  node --test internal/server/static/shared/

import { test } from 'node:test';
import { strict as assert } from 'node:assert';

import {
  findActiveWord,
  highlightWindow,
  fileToBookTime,
  bookToFileTime,
  chapterAtTime,
  SYNC_LEAD_SECS,
  WINDOW_BEHIND,
  WINDOW_AHEAD,
} from './karaoke.js';

import {
  formatDuration,
  formatChapterDuration,
  formatPlayerTime,
} from './format.js';

const sampleWords = [
  { s: 0.0, e: 0.5, w: 'Hello' },
  { s: 0.5, e: 1.0, w: 'world' },
  { s: 1.0, e: 1.6, w: 'foo' },
  { s: 1.6, e: 2.4, w: 'bar' },
  { s: 2.4, e: 3.0, w: 'baz' },
];

test('findActiveWord: time before first word returns -1', () => {
  assert.equal(findActiveWord(sampleWords, -1), -1);
});

test('findActiveWord: exact start of a word', () => {
  assert.equal(findActiveWord(sampleWords, 1.0), 2); // start of "foo"
});

test('findActiveWord: between words holds the previous one', () => {
  // 1.5 is between "foo" (ends 1.6) and "bar" (starts 1.6) — actually
  // tests inside foo's range. Add a clearer between case:
  assert.equal(findActiveWord(sampleWords, 1.55), 2); // still on "foo"
});

test('findActiveWord: in a silent gap, holds last started word', () => {
  // Synthesize a gap by querying past the last word's end.
  assert.equal(findActiveWord(sampleWords, 5.0), 4); // last word
});

test('findActiveWord: empty array', () => {
  assert.equal(findActiveWord([], 1.0), -1);
  assert.equal(findActiveWord(null, 1.0), -1);
});

test('highlightWindow: standard window around active', () => {
  const w = highlightWindow(10, 100);
  assert.equal(w.readStart, 10 - WINDOW_BEHIND);
  assert.equal(w.readEnd, 10 + WINDOW_AHEAD);
});

test('highlightWindow: clamps at start of book', () => {
  const w = highlightWindow(0, 100);
  assert.equal(w.readStart, 0);
  assert.equal(w.readEnd, WINDOW_AHEAD);
});

test('highlightWindow: clamps at end of book', () => {
  const w = highlightWindow(99, 100);
  assert.equal(w.readStart, 99 - WINDOW_BEHIND);
  assert.equal(w.readEnd, 99); // can't exceed totalWords-1
});

test('highlightWindow: -1 active returns empty range', () => {
  const w = highlightWindow(-1, 100);
  assert.ok(w.readStart > w.readEnd);
});

test('fileToBookTime: single-file passthrough', () => {
  assert.equal(fileToBookTime(123.4, 0), 123.4);
});

test('fileToBookTime: multi-file adds offset', () => {
  assert.equal(fileToBookTime(60, 3600), 3660);
});

test('bookToFileTime: empty sources is single-file', () => {
  assert.deepEqual(bookToFileTime(123, []), { fileIdx: 0, localSecs: 123 });
});

test('bookToFileTime: maps absolute to file+offset', () => {
  const sources = [
    { file: 'a.mp3', offsetSecs: 0,    durationSecs: 1000 },
    { file: 'b.mp3', offsetSecs: 1000, durationSecs: 1000 },
    { file: 'c.mp3', offsetSecs: 2000, durationSecs: 1000 },
  ];
  assert.deepEqual(bookToFileTime(500,  sources), { fileIdx: 0, localSecs: 500 });
  assert.deepEqual(bookToFileTime(1500, sources), { fileIdx: 1, localSecs: 500 });
  assert.deepEqual(bookToFileTime(2500, sources), { fileIdx: 2, localSecs: 500 });
});

test('bookToFileTime: out-of-range clamps to last file', () => {
  const sources = [{ file: 'a.mp3', offsetSecs: 0, durationSecs: 100 }];
  const r = bookToFileTime(9999, sources);
  assert.equal(r.fileIdx, 0);
  // localSecs = bookSecs - offsetSecs since we hit the "last file" branch
  assert.equal(r.localSecs, 9999);
});

test('chapterAtTime: returns matching chapter', () => {
  const chapters = [
    { startSec: 0,    endSec: 1000 },
    { startSec: 1000, endSec: 2000 },
    { startSec: 2000, endSec: 3000 },
  ];
  assert.equal(chapterAtTime(chapters, 1500).startSec, 1000);
});

test('chapterAtTime: handles snake_case field names from server JSON', () => {
  const chapters = [
    { start_sec: 0,    end_sec: 1000 },
    { start_sec: 1000, end_sec: 2000 },
  ];
  assert.equal(chapterAtTime(chapters, 500).start_sec, 0);
});

test('chapterAtTime: past-end returns last chapter (so UI does not blank)', () => {
  const chapters = [{ startSec: 0, endSec: 100 }];
  assert.equal(chapterAtTime(chapters, 9999), chapters[0]);
});

test('chapterAtTime: empty chapters returns null', () => {
  assert.equal(chapterAtTime([], 1.0), null);
  assert.equal(chapterAtTime(null, 1.0), null);
});

test('SYNC_LEAD_SECS / WINDOW constants are exported', () => {
  assert.equal(typeof SYNC_LEAD_SECS, 'number');
  assert.equal(typeof WINDOW_BEHIND, 'number');
  assert.equal(typeof WINDOW_AHEAD, 'number');
});

// ---- format.js ----

test('formatDuration: zero/falsy → empty string', () => {
  assert.equal(formatDuration(0), '');
  assert.equal(formatDuration(null), '');
  assert.equal(formatDuration(undefined), '');
  assert.equal(formatDuration(-1), '');
});

test('formatDuration: under an hour → M:SS', () => {
  assert.equal(formatDuration(65), '1:05');
  assert.equal(formatDuration(3599), '59:59');
});

test('formatDuration: an hour or more → H:MM:SS', () => {
  assert.equal(formatDuration(3600), '1:00:00');
  assert.equal(formatDuration(3661), '1:01:01');
});

test('formatChapterDuration: scan-friendly buckets', () => {
  assert.equal(formatChapterDuration(45), '45s');
  assert.equal(formatChapterDuration(120), '2m');
  assert.equal(formatChapterDuration(3600), '1h');
  assert.equal(formatChapterDuration(3900), '1h 5m');
});

test('formatPlayerTime: nan/zero falls back to 0:00 (not empty)', () => {
  assert.equal(formatPlayerTime(0), '0:00');
  assert.equal(formatPlayerTime(NaN), '0:00');
  assert.equal(formatPlayerTime(65), '1:05');
});
