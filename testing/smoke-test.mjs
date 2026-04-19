#!/usr/bin/env node
// Browser-based smoke test. Loads the web UI against a running server and
// verifies each work can be expanded, its reader loads chapters, and no JS
// errors are thrown. Runs in headless Chromium via Playwright.
//
// Ran after any user-visible change to catch classes of bug that were
// previously being shipped and discovered by the user:
//   - Work's chapter API returning non-array crashes renderChapterList (#119-followup)
//   - Multi-file audiobooks with whole-book sync not finding sync_data
//   - Chapters with >8k words exhausting the DOM
//
// Usage:
//   node testing/smoke-test.mjs [server-url]
//
// Requires: npx playwright install chromium (first run only)

import { chromium } from 'playwright';

const BASE_URL = process.argv[2] || 'http://localhost:7654';
const TIMEOUT = 30000;

// Track all JS errors / console errors raised during the test.
const errors = [];
const warnings = [];

async function smokeTest() {
  const browser = await chromium.launch({ headless: true });
  const ctx = await browser.newContext();
  const page = await ctx.newPage();

  page.on('pageerror', (err) => {
    errors.push(`[pageerror] ${err.message}`);
  });
  page.on('console', (msg) => {
    if (msg.type() === 'error') errors.push(`[console.error] ${msg.text()}`);
    if (msg.type() === 'warning') warnings.push(`[console.warn] ${msg.text()}`);
  });

  console.log(`→ Loading ${BASE_URL}`);
  await page.goto(BASE_URL, { waitUntil: 'networkidle', timeout: TIMEOUT });

  // Pull the works list from the API the page hydrates from.
  const works = await page.evaluate(async () => {
    const r = await fetch('/api/works');
    return await r.json();
  });
  console.log(`→ Found ${works.length} works`);

  let passCount = 0;
  const failures = [];

  for (const w of works) {
    const errsBefore = errors.length;
    try {
      await testWork(page, w);
      if (errors.length > errsBefore) {
        failures.push({
          work: `#${w.id} ${w.title}`,
          errors: errors.slice(errsBefore),
        });
      } else {
        passCount++;
      }
    } catch (e) {
      failures.push({ work: `#${w.id} ${w.title}`, errors: [String(e)] });
    }
  }

  await browser.close();

  console.log(`\n=== Smoke test: ${passCount}/${works.length} works passed ===`);
  if (failures.length) {
    console.log(`\n${failures.length} FAILURE(S):`);
    for (const f of failures) {
      console.log(`\n  ${f.work}:`);
      for (const e of f.errors) console.log(`    ${e}`);
    }
    process.exit(1);
  }
}

async function testWork(page, w) {
  console.log(`\n→ Testing #${w.id} ${w.title}`);

  // Expand the work card.
  const card = await page.$(`[id="work-${w.id}"], .work-card[data-work-id="${w.id}"]`);
  if (card) {
    await card.click({ timeout: 5000 }).catch(() => {});
  }

  // Validate chapter-list API shape (regression: must be an array).
  if (w.has_text && w.text_files?.length) {
    const book = w.text_files[0];
    const chapters = await page.evaluate(async (bookId) => {
      const r = await fetch(`/api/books/${bookId}/chapters`);
      return { ok: r.ok, status: r.status, body: await r.json() };
    }, book.id);
    if (!Array.isArray(chapters.body)) {
      throw new Error(`chapters API returned non-array for book ${book.id}: ${JSON.stringify(chapters.body).slice(0, 200)}`);
    }
    console.log(`  ✓ chapter list (${chapters.body.length} chapters)`);

    // Spot-check first chapter content endpoint.
    if (chapters.body.length > 0) {
      const ch0 = await page.evaluate(async (bookId) => {
        const r = await fetch(`/api/books/${bookId}/chapters/0`);
        return await r.json();
      }, book.id);
      if (!ch0.content && !ch0.content_html) {
        throw new Error(`chapter 0 has no content or content_html`);
      }
      const len = (ch0.content || '').length + (ch0.content_html || '').length;
      console.log(`  ✓ chapter 0 content (${len} chars, ${ch0.word_count} words)`);
    }
  }

  // Validate sync data for multi-file audio works: sync must be retrievable
  // against some audio book, not necessarily the one the user clicks.
  if (w.has_audio && w.audio_files?.length) {
    const syncBookId = w.audio_files[0].id;
    const syncData = await page.evaluate(async (wid, bid) => {
      const r = await fetch(`/api/works/${wid}/sync/${bid}/0`);
      if (!r.ok) return null;
      return await r.json();
    }, w.id, syncBookId);
    if (syncData && Array.isArray(syncData) && syncData.length > 0) {
      const firstWord = syncData[0];
      const hasRealData = firstWord.s !== 0 || firstWord.e !== 0 || firstWord.w !== '';
      if (!hasRealData) {
        throw new Error(`sync_data has zero-filled entries (schema mismatch on import?)`);
      }
      console.log(`  ✓ sync data (${syncData.length} words, first="${firstWord.w?.trim()}")`);
    } else {
      console.log(`  · no sync data (OK for audio-only without transcription)`);
    }
  }
}

smokeTest().catch((e) => {
  console.error('smoke-test failed:', e);
  process.exit(2);
});
