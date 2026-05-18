// Unit tests for the reader DOM helpers. Pure-JS, runnable with
// Node's built-in test runner (same as karaoke.test.js):
//
//   node --test internal/server/static/shared/

import { test } from 'node:test';
import { strict as assert } from 'node:assert';

import {
  paragraphsFromText,
  htmlForPlainText,
  extractReaderParagraphs,
  transcriptParagraphsHTML,
} from './reader.js';

// Tiny mock for a reader DOM element. Just enough surface to exercise
// extractReaderParagraphs: querySelectorAll('p') + textContent. We're
// not pulling in jsdom for this one function.
function fakeReader(html) {
  const psPattern = /<p[^>]*>([\s\S]*?)<\/p>/g;
  const ps = [];
  let m;
  while ((m = psPattern.exec(html)) !== null) {
    ps.push({ textContent: m[1] });
  }
  return {
    querySelectorAll(sel) {
      return sel === 'p' ? ps : [];
    },
    // Browser textContent strips tags AND concatenates child text nodes
    // with NO separator — modeling that is the whole point of this test.
    get textContent() {
      return html.replace(/<[^>]+>/g, '');
    },
  };
}

// ---- paragraphsFromText ----

test('paragraphsFromText: splits on double-newline', () => {
  const got = paragraphsFromText('one\n\ntwo\n\nthree');
  assert.deepEqual(got, ['one', 'two', 'three']);
});

test('paragraphsFromText: collapses 3+ newlines too', () => {
  const got = paragraphsFromText('one\n\n\n\ntwo');
  assert.deepEqual(got, ['one', 'two']);
});

test('paragraphsFromText: drops empty-string paragraphs', () => {
  const got = paragraphsFromText('\n\nhello\n\n\n\n');
  assert.deepEqual(got, ['hello']);
});

test('paragraphsFromText: empty / null', () => {
  assert.deepEqual(paragraphsFromText(''), []);
  assert.deepEqual(paragraphsFromText(null), []);
});

// ---- htmlForPlainText ----

test('htmlForPlainText: wraps each paragraph in <p>', () => {
  const got = htmlForPlainText('one\n\ntwo');
  assert.equal(got, '<p>one</p><p>two</p>');
});

test('htmlForPlainText: within-paragraph newlines become <br>', () => {
  const got = htmlForPlainText('line a\nline b\n\nnext para');
  assert.equal(got, '<p>line a<br>line b</p><p>next para</p>');
});

test('htmlForPlainText: escapes HTML metacharacters', () => {
  const got = htmlForPlainText('a < b & "c"');
  assert.equal(got, '<p>a &lt; b &amp; &quot;c&quot;</p>');
});

test('htmlForPlainText: empty text → empty string (not "<p></p>")', () => {
  assert.equal(htmlForPlainText(''), '');
  assert.equal(htmlForPlainText('  '), '');
});

// ---- extractReaderParagraphs — the #164 regression test ----

test('extractReaderParagraphs: prefers <p> children when present (#164)', () => {
  // The bug: textContent on a reader with <p>foo.</p><p>bar.</p>
  // returns "foo.bar." (no space between). If we split that on \n\n
  // we get ONE giant paragraph instead of two.
  const reader = fakeReader('<p>foo.</p><p>bar.</p>');
  const got = extractReaderParagraphs(reader);
  assert.deepEqual(got, ['foo.', 'bar.']);
});

test('extractReaderParagraphs: falls back to textContent split when no <p>', () => {
  const reader = fakeReader('alpha\n\nbeta\n\ngamma');
  const got = extractReaderParagraphs(reader);
  assert.deepEqual(got, ['alpha', 'beta', 'gamma']);
});

test('extractReaderParagraphs: skips empty <p> elements', () => {
  const reader = fakeReader('<p>one</p><p>  </p><p>two</p>');
  const got = extractReaderParagraphs(reader);
  assert.deepEqual(got, ['one', 'two']);
});

test('extractReaderParagraphs: handles null reader', () => {
  assert.deepEqual(extractReaderParagraphs(null), []);
});

// ---- transcriptParagraphsHTML ----

test('transcriptParagraphsHTML: builds widx-anchored <p> elements', () => {
  const paras = ['One two three.', 'Four five.'];
  const timestamps = new Array(5); // 5 words total
  const got = transcriptParagraphsHTML(paras, timestamps);
  assert.equal(got.paragraphCount, 2);
  assert.equal(got.totalWords, 5);
  assert.equal(got.karaokeSafe, true);
  assert.match(got.html, /data-widx-start="0".*data-widx-end="3"/);
  assert.match(got.html, /data-widx-start="3".*data-widx-end="5"/);
});

test('transcriptParagraphsHTML: flags unsafe when word counts diverge >5%', () => {
  const paras = ['one two', 'three four']; // 4 words
  const timestamps = new Array(20);         // sync_data says 20
  const got = transcriptParagraphsHTML(paras, timestamps);
  assert.equal(got.karaokeSafe, false);
});

test('transcriptParagraphsHTML: stays safe inside tolerance', () => {
  // 100 words across paragraphs, sync says 102 — within 5% tolerance
  const paras = [Array(50).fill('w').join(' '), Array(50).fill('w').join(' ')];
  const got = transcriptParagraphsHTML(paras, new Array(102));
  assert.equal(got.karaokeSafe, true);
});

test('transcriptParagraphsHTML: escapes HTML in paragraph text', () => {
  const paras = ['<script>alert(1)</script>'];
  const got = transcriptParagraphsHTML(paras, new Array(2));
  assert.ok(!got.html.includes('<script>'), 'must escape <script>');
  assert.ok(got.html.includes('&lt;script&gt;'));
});

test('transcriptParagraphsHTML: empty input', () => {
  const got = transcriptParagraphsHTML([], []);
  assert.equal(got.paragraphCount, 0);
  assert.equal(got.karaokeSafe, false);
});
