// Karaoke word-sync logic shared by web (browser) and mobile (RN/Hermes).
// Pure ES module — no DOM, no React Native imports. The platform-specific
// view layers consume the outputs of these functions to render highlights,
// scroll, etc.
//
// Sync data shape: an array of word records {s, e, w} where s/e are start
// and end times in seconds (book-absolute) and w is the original word
// text. The array is monotonic in s — we rely on that for binary search.

/**
 * Soft reading-window radius and lead time, tuned over Bonfire/PHM/WWS.
 * Both clients use the same constants so karaoke "feels" identical.
 *
 * - SYNC_LEAD_SECS: shift the lookup forward by this much because Whisper
 *   timestamps tend to land very slightly after the audible word onset.
 *   User-configurable on web (the "Lead (ms)" input above the reader).
 * - WINDOW_BEHIND / WINDOW_AHEAD: count of words around the active one
 *   that get the "currently being read" highlight. Words past the window
 *   fade; words future-of-active are not yet styled.
 */
export const SYNC_LEAD_SECS = 0.35;
export const WINDOW_BEHIND = 1;
export const WINDOW_AHEAD = 4;

/**
 * Find the active word index for a given playback time. Returns the
 * index of the last word whose start <= time, or -1 if time is before
 * the first word. Uses binary search — O(log n).
 *
 * The lookup is "last word that started" rather than "word containing
 * time" so we degrade gracefully through gaps between words (silences
 * inside the audio, where no word covers the current time): the most
 * recent word stays highlighted until the next one starts.
 */
export function findActiveWord(words, timeSecs) {
  if (!words || words.length === 0) return -1;
  let lo = 0, hi = words.length - 1, ans = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (words[mid].s <= timeSecs) {
      ans = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return ans;
}

/**
 * Compute the soft-reading window around the active word. Returns
 * `{readStart, readEnd}` (both inclusive, both clamped to [0, n-1]).
 * Word indices in [readStart, readEnd] get the "actively reading" style;
 * indices < readStart get the "already past" style.
 *
 * If activeIdx is -1 the window is empty (returns readStart > readEnd).
 */
export function highlightWindow(activeIdx, totalWords, behind = WINDOW_BEHIND, ahead = WINDOW_AHEAD) {
  if (activeIdx < 0 || totalWords <= 0) {
    return { readStart: 0, readEnd: -1 };
  }
  return {
    readStart: Math.max(0, activeIdx - behind),
    readEnd: Math.min(totalWords - 1, activeIdx + ahead),
  };
}

/**
 * Convert a position within a single audio file into book-absolute time
 * for multi-file audiobooks. `offsetSecs` is where this file's audio
 * starts in the concatenated timeline. Returns `localSecs + offsetSecs`.
 *
 * For single-file audiobooks pass offsetSecs=0 and this is a no-op.
 */
export function fileToBookTime(localSecs, offsetSecs) {
  return (localSecs || 0) + (offsetSecs || 0);
}

/**
 * Inverse of fileToBookTime — given a book-absolute time and the source
 * timeline (array of `{file, offsetSecs, durationSecs}`), find which
 * file the time falls in and the offset within that file. Returns
 * `{fileIdx, localSecs}`. If sources is missing/empty, treats as a
 * single-file timeline starting at 0.
 *
 * Out-of-range times clamp to the last file's end, NOT to a negative
 * answer — playback at end-of-book stays on the last file.
 */
export function bookToFileTime(bookSecs, sources) {
  if (!sources || sources.length === 0) {
    return { fileIdx: 0, localSecs: bookSecs };
  }
  for (let i = 0; i < sources.length; i++) {
    const src = sources[i];
    const start = src.offsetSecs || 0;
    const end = start + (src.durationSecs || 0);
    if (bookSecs < end || i === sources.length - 1) {
      return { fileIdx: i, localSecs: Math.max(0, bookSecs - start) };
    }
  }
  // Unreachable — for-loop always returns on the last iteration.
  return { fileIdx: sources.length - 1, localSecs: 0 };
}

/**
 * Find the chapter containing a given time. Chapters are
 * `{startSec, endSec, ...}` and are assumed sorted by startSec and
 * non-overlapping. Returns the chapter object, or null if none match.
 *
 * Used by both clients to drive "currently in chapter N" UI.
 */
export function chapterAtTime(chapters, timeSecs) {
  if (!chapters || chapters.length === 0) return null;
  for (const ch of chapters) {
    const start = ch.startSec || ch.start_sec || 0;
    const end = ch.endSec || ch.end_sec || 0;
    if (timeSecs >= start && (end === 0 || timeSecs < end)) {
      return ch;
    }
  }
  // After the last chapter — return it so UIs that show "current chapter"
  // don't blank out at end-of-book.
  return chapters[chapters.length - 1];
}
