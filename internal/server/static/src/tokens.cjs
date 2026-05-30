// Shared design tokens (#205 / Phase 1 #144). Single source of truth for
// the Abookify design system, mirroring STYLE_GUIDE.md +
// design/design-system-mock.html.
//
// Web (this repo) feeds these into tailwind.config.js. The web theme maps
// *colors* to CSS custom properties (var(--x)) so light/dark switches at
// :root; the raw light/dark hex below are the values those vars take and
// are what the MOBILE app (NativeWind) consumes directly. Spacing / radius
// / fonts are shared verbatim by both.
//
// Cross-repo sharing is copy-mirror for now (web and mobile are separate
// repos, no shared package): keep this file and mobile's copy in step, and
// treat STYLE_GUIDE.md as the human-readable source of truth.

const color = {
  light: {
    bg: '#FFFFFF',
    surface: '#FFFFFF',
    'surface-2': '#F8FAFC',
    text: '#111827',
    muted: '#6A7280',
    'muted-2': '#9CA3AF',
    'text-soft': '#374151',
    accent: '#1D4ED8',
    'accent-soft': 'rgba(29,78,216,.12)',
    border: '#E5E7EB',
    danger: '#E94560',
    'danger-soft': 'rgba(233,69,96,.12)',
    success: '#0F766E',
    'success-soft': 'rgba(15,118,110,.14)',
    highlight: '#DBEAFE',
    'ebook-only': '#F59E0B',
    'ebook-only-soft': 'rgba(245,158,11,.12)',
    // Mobile-introduced 2026-05-29 (#205 legacy-color tokenization).
    // Web's index.html already used --epub/--mp3 as inline vars;
    // promoted here so both clients reach the same names. Light values
    // are first-pass — refine when web adopts these in STYLE_GUIDE.
    epub: '#0D8866',
    'epub-soft': 'rgba(13,136,102,.12)',
    mp3: '#B45309',
    'karaoke-read': '#111827',
    'karaoke-past': '#D1D5DB',
  },
  dark: {
    bg: '#0B1020',
    surface: '#141A2E',
    'surface-2': '#111827',
    text: '#F3F4F6',
    muted: '#9AA0B0',
    'muted-2': '#55607a',
    'text-soft': '#c0c8d8',
    accent: '#3B82F6',
    'accent-soft': 'rgba(59,130,246,.16)',
    border: '#1F2937',
    danger: '#E94560',
    'danger-soft': 'rgba(233,69,96,.12)',
    success: '#34D399',
    'success-soft': 'rgba(52,211,153,.14)',
    highlight: '#172554',
    'ebook-only': '#F59E0B',
    'ebook-only-soft': 'rgba(245,158,11,.12)',
    // Mobile-introduced 2026-05-29 (#205 legacy-color tokenization).
    // Mirrors web's existing inline --epub/--mp3 conventions.
    epub: '#4ecca3',
    'epub-soft': 'rgba(78,204,163,.15)',
    mp3: '#f0a500',
    'karaoke-read': '#e8eaf0',
    'karaoke-past': '#3a4260',
  },
};

// Spacing scale 4→40 (keys match --space-N).
const space = {
  1: '4px', 2: '8px', 3: '12px', 4: '16px',
  5: '20px', 6: '24px', 7: '32px', 8: '40px',
};

const radius = { sm: '10px', md: '16px', lg: '24px' };

const font = {
  ui: ['Inter', 'ui-sans-serif', 'system-ui', '-apple-system', 'BlinkMacSystemFont', '"Segoe UI"', 'Roboto', 'sans-serif'],
  reader: ['Georgia', '"Iowan Old Style"', '"Charter"', 'ui-serif', 'serif'],
};

module.exports = { color, space, radius, font };
