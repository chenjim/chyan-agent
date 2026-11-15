// Brand mark: a chat bubble with a terminal prompt, drawn as pure vectors so it
// stays crisp from a 16px favicon to the 44px login tile. Uid keeps the SVG
// gradient id unique when several marks share one document.

const BRAND_FROM = '#5b74ff';
const BRAND_TO = '#3a55e0';
const BRAND_INK = '#3a55e0';

/** Return the inline SVG for one brand tile (background + bubble + `>_`). */
export function brandMarkSvg(uid: string): string {
  return `<svg class="brand-svg" viewBox="0 0 64 64" aria-hidden="true" focusable="false">
  <defs>
    <linearGradient id="brand-g-${uid}" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="${BRAND_FROM}"/>
      <stop offset="1" stop-color="${BRAND_TO}"/>
    </linearGradient>
  </defs>
  <rect width="64" height="64" rx="16" fill="url(#brand-g-${uid})"/>
  <path d="M18 43 L18 53 L29 43 Z" fill="#ffffff"/>
  <rect x="11" y="12" width="42" height="32" rx="11" fill="#ffffff"/>
  <path d="M23 22.5 L30 28 L23 33.5" fill="none" stroke="${BRAND_INK}"
        stroke-width="4.5" stroke-linecap="round" stroke-linejoin="round"/>
  <path d="M35 34.5 H41.5" fill="none" stroke="${BRAND_INK}"
        stroke-width="4.5" stroke-linecap="round"/>
</svg>`;
}
