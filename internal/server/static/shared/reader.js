// Reader DOM helpers: the building blocks for rendering chapter
// content + transcript-paragraph layout into a karaoke-ready surface.
//
// Pure functions (text → text) live here so the same logic can be
// unit-tested without a browser and reused by the mobile reader if
// it ever stops doing its own thing. The one DOM-aware helper
// (extractReaderParagraphs) takes a root element argument so callers
// can pass a stub in tests.
//
// History note (#164): the original transcript renderer recovered
// paragraphs by reading `reader.textContent` and splitting on \n\n.
// That broke the day renderChapterContent started wrapping plain text
// in <p> — textContent fuses <p> nodes with no separator, so a six-
// paragraph chapter came back as one giant blob. extractReaderParagraphs
// prefers existing <p> children when present, with the textContent
// split as a fallback for unwrapped content. The tests in
// reader.test.js exercise both paths and a 12-line DOM stub stands in
// for jsdom.

/**
 * Wrap plain text in <p> blocks for the reader. Single newlines
 * within a paragraph become <br>; paragraph boundaries are \n\n+.
 * Returns HTML string. Empty text returns "".
 */
export function htmlForPlainText(text) {
  const paras = paragraphsFromText(text);
  if (paras.length === 0) return '';
  return paras
    .map(p => '<p>' + escapeHTML(p).replace(/\n/g, '<br>') + '</p>')
    .join('');
}

/**
 * Split a plain-text string into paragraph strings by \n\n+
 * boundaries. Empty paragraphs filtered out.
 */
export function paragraphsFromText(text) {
  return (text || '').split(/\n\n+/).map(p => p).filter(p => p.trim().length > 0);
}

/**
 * Read paragraph text array from a reader element. Prefers existing
 * <p> children — the path the transcript renderer relies on after
 * renderChapterContent has wrapped plain text. Falls back to
 * textContent split on \n\n+ when no <p> tags are present (the rare
 * case where content was loaded as a bare text node).
 *
 * IMPORTANT: do NOT inline this with `reader.textContent.split(...)`.
 * textContent concatenates child text nodes with no separator, so
 * adjacent <p> elements lose their boundaries — the #164 regression.
 */
export function extractReaderParagraphs(reader) {
  if (!reader) return [];
  const ps = reader.querySelectorAll ? reader.querySelectorAll('p') : [];
  if (ps && ps.length > 0) {
    const out = [];
    for (let i = 0; i < ps.length; i++) {
      const t = (ps[i].textContent || '').trim();
      if (t.length > 0) out.push(t);
    }
    return out;
  }
  const raw = (reader.textContent || '').toString();
  return raw.split(/\n\n+/).filter(p => p.trim().length > 0);
}

/**
 * Build the synced-transcript HTML from an array of paragraph texts.
 * Each paragraph carries data-widx-start / data-widx-end attributes
 * so the karaoke loop can find the right one to wrap when the active
 * word enters it.
 *
 * Returns:
 *   {
 *     html: string,          // ready for reader.innerHTML
 *     totalWords: number,    // sum of words across paragraphs
 *     paragraphCount: number,
 *     karaokeSafe: boolean,  // false if paragraph word counts diverge
 *                            //   from `timestamps.length` by more than 5%
 *     paragraphs: [{ widxStart, widxEnd, wordCount }]
 *   }
 *
 * The divergence check is the same one renderParagraphedTranscript
 * has used since the bounded-DOM karaoke landed: when the per-
 * paragraph word counts drift from sync_data's word count by >5%, the
 * karaoke loop's word-index lookups will land on the wrong word, so
 * we fall back to non-sync rendering.
 */
export function transcriptParagraphsHTML(paraTexts, timestamps) {
  const paragraphs = [];
  let widx = 0;
  for (const pt of paraTexts) {
    const trimmed = pt.trim();
    if (trimmed.length === 0) continue;
    const wordCount = trimmed.split(/\s+/).length;
    paragraphs.push({ widxStart: widx, widxEnd: widx + wordCount, wordCount, text: trimmed });
    widx += wordCount;
  }
  const tsLen = (timestamps && timestamps.length) || 0;
  const tolerance = tsLen * 0.05;
  const karaokeSafe = paragraphs.length > 0 &&
    Math.abs(widx - tsLen) <= Math.max(tolerance, 1);
  const html = paragraphs
    .map(p =>
      `<p class="sync-para" data-widx-start="${p.widxStart}" data-widx-end="${p.widxEnd}">` +
      escapeHTML(p.text) + '</p>')
    .join('');
  return {
    html,
    totalWords: widx,
    paragraphCount: paragraphs.length,
    karaokeSafe,
    paragraphs: paragraphs.map(p => ({
      widxStart: p.widxStart,
      widxEnd: p.widxEnd,
      wordCount: p.wordCount,
    })),
  };
}

/**
 * Build karaoke-ready transcript HTML directly from the sync timestamps
 * — NOT from the displayed text chapter. This is the alignment-correct
 * path: Whisper's word tokens and the curated transcript text tokenize
 * differently (Whisper splits possessives/contractions — "O'Halloran"
 * becomes "O" + "'Halloran"), so the old approach of counting displayed
 * words and mapping sync[i] onto displayed-word[i] drifts further ahead
 * the deeper you read (measured ~23% word-count divergence on a 68k-word
 * audiobook — enough to put the highlight ~3s ahead of the audio mid-
 * chapter). Rendering the sync words themselves makes data-widx == the
 * timestamp index by construction, which is exactly what the mobile
 * NowPlaying karaoke does — and why mobile never drifts.
 *
 * Words are grouped into paragraphs at pause boundaries (gap between one
 * word's end and the next word's start > `gapSecs`), mirroring the
 * sidecar importer's >0.6s paragraphing, with `maxWords` as a safety cap
 * so a gapless stretch can't become a wall of text. Each word is emitted
 * pre-wrapped as <span class="sync-word" data-widx="i">; the paragraph
 * carries data-wrapped="1" so the lazy wrapper no-ops over it.
 *
 * Returns the same shape as transcriptParagraphsHTML. karaokeSafe is
 * always true here — the mapping cannot diverge because both sides ARE
 * the timestamp array.
 */
export function transcriptParagraphsFromSync(timestamps, opts) {
  const o = opts || {};
  const gapSecs = o.gapSecs != null ? o.gapSecs : 0.6;
  const maxWords = o.maxWords != null ? o.maxWords : 150;
  const n = (timestamps && timestamps.length) || 0;
  if (n === 0) {
    return { html: '', totalWords: 0, paragraphCount: 0, karaokeSafe: false, paragraphs: [] };
  }
  // Paragraph boundaries: [start, endExclusive) index ranges.
  const bounds = [];
  let start = 0;
  for (let i = 1; i < n; i++) {
    const gap = (timestamps[i].s || 0) - (timestamps[i - 1].e || 0);
    if (gap > gapSecs || (i - start) >= maxWords) {
      bounds.push([start, i]);
      start = i;
    }
  }
  bounds.push([start, n]);

  const paragraphs = [];
  let html = '';
  for (let b = 0; b < bounds.length; b++) {
    const s = bounds[b][0];
    const e = bounds[b][1];
    let inner = '';
    for (let i = s; i < e; i++) {
      const w = String((timestamps[i] && timestamps[i].w) || '').trim();
      inner += '<span class="sync-word" data-widx="' + i + '">' + escapeHTML(w) + '</span>';
      if (i < e - 1) inner += ' ';
    }
    if (inner.length === 0) continue;
    html += '<p class="sync-para" data-widx-start="' + s + '" data-widx-end="' + e +
      '" data-wrapped="1">' + inner + '</p>';
    paragraphs.push({ widxStart: s, widxEnd: e, wordCount: e - s });
  }
  return {
    html,
    totalWords: n,
    paragraphCount: paragraphs.length,
    karaokeSafe: true,
    paragraphs,
  };
}

function escapeHTML(s) {
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
