// Time-formatting helpers shared by web (browser) and mobile (RN/Hermes).
// Pure ES module — no DOM, no React Native imports — so it loads in either
// runtime unchanged. Imported by both clients via @shared/format.
//
// Source of truth: this file. Web reaches it via go:embed; mobile reaches
// it via a tsconfig path alias to the same path.

/**
 * Format seconds as `H:MM:SS` (when ≥1h) or `M:SS`. Used in the audio
 * player time display, the bookmarks list, and other places that need
 * a precise running clock.
 *
 * Returns "" for falsy/non-positive input so the caller can render
 * conditionally without a special check.
 */
export function formatDuration(secs) {
  if (!secs || secs <= 0) return '';
  const h = Math.floor(secs / 3600);
  const m = Math.floor((secs % 3600) / 60);
  const s = Math.floor(secs % 60);
  if (h > 0) {
    return `${h}:${m.toString().padStart(2, '0')}:${s.toString().padStart(2, '0')}`;
  }
  return `${m}:${s.toString().padStart(2, '0')}`;
}

/**
 * Compact, scan-friendly duration for TOC rows: "12m", "1h 4m", "47s".
 * Designed for at-a-glance reading — exact seconds aren't useful at the
 * chapter scale.
 */
export function formatChapterDuration(secs) {
  if (!secs || secs <= 0) return '';
  const s = Math.round(secs);
  if (s < 60) return s + 's';
  // Floor the minutes, like the player's clock (formatTimeHuman) does: the
  // chapter list's "15h 59m total" beside a transport reading "15h 58m" was two
  // formatters rounding differently, and a stranger reads the gap as missing
  // audio (card 36). One rule, shared with mobile through this file.
  const m = Math.floor(s / 60);
  if (m < 60) return m + 'm';
  const h = Math.floor(m / 60);
  const rem = m - h * 60;
  return rem > 0 ? `${h}h ${rem}m` : `${h}h`;
}

/**
 * Player-bar duration: "0:00" fallback for nan/0 instead of empty string.
 * Used where the UI always shows a clock and "" would look broken.
 */
export function formatPlayerTime(secs) {
  if (!secs || isNaN(secs)) return '0:00';
  return formatDuration(secs) || '0:00';
}
