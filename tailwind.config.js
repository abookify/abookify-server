// Tailwind config for the embedded web UI (#205 / Phase 1 #144).
//
// Build: `make css` runs the Tailwind v3 standalone via Docker (no host
// toolchain) and writes the minified output to
// internal/server/static/app.css, which is committed + go:embed'd.
//
// Colors map to the CSS custom properties defined at :root in the page
// <style> blocks, so the same utility (e.g. bg-surface) follows the
// light/dark theme switch automatically. Spacing / radius / fonts come
// from the shared tokens module that mobile (NativeWind) also consumes.
const tokens = require('./internal/server/static/src/tokens.cjs');

// CSS-var-backed colors: one entry per semantic token. Utilities resolve
// to var(--token); the actual hex lives at :root (and its dark override).
const cssVarColors = Object.keys(tokens.color.dark).reduce((acc, name) => {
  acc[name] = `var(--${name})`;
  return acc;
}, {});

module.exports = {
  content: [
    './internal/server/static/index.html',
    './internal/server/static/settings.html',
    './internal/server/static/tts-compare.html',
    './internal/server/static/shared/**/*.js',
  ],
  // The app keeps its own reset (`* { margin:0; padding:0 }`); Tailwind's
  // preflight would fight the large hand-written stylesheet, so it stays
  // off — same as the prior CDN config.
  corePlugins: { preflight: false },
  theme: {
    extend: {
      colors: cssVarColors,
      spacing: tokens.space,
      borderRadius: tokens.radius,
      fontFamily: {
        ui: tokens.font.ui,
        reader: tokens.font.reader,
      },
      boxShadow: {
        surface: 'var(--shadow)',
      },
    },
  },
  plugins: [],
};
