// Theme preference: light, dark, or follow the OS. The resolved value is
// written to <html data-theme> so CSS never has to duplicate media queries.

export type Theme = 'light' | 'dark' | 'system';
export type ResolvedTheme = 'light' | 'dark';

export const THEME_KEY = 'chyan_theme';
export const THEMES: readonly { id: Theme; label: string }[] = [
  { id: 'light', label: '浅色' },
  { id: 'dark', label: '深色' },
  { id: 'system', label: '跟随系统' },
];

const COLORS: Record<ResolvedTheme, string> = {
  light: '#f7f7f8',
  dark: '#18181a',
};

/** Read the stored preference; anything unknown falls back to system. */
export function loadTheme(): Theme {
  const raw = localStorage.getItem(THEME_KEY);
  return raw === 'light' || raw === 'dark' || raw === 'system' ? raw : 'system';
}

/** Resolve a preference against the current OS colour scheme. */
export function resolveTheme(theme: Theme, prefersDark: boolean): ResolvedTheme {
  if (theme === 'system') return prefersDark ? 'dark' : 'light';
  return theme;
}

export function prefersDark(): boolean {
  return typeof matchMedia === 'function' && matchMedia('(prefers-color-scheme: dark)').matches;
}

/** Apply one preference and keep the browser chrome colour in sync. */
export function applyTheme(theme: Theme): ResolvedTheme {
  const resolved = resolveTheme(theme, prefersDark());
  document.documentElement.dataset.theme = resolved;
  const meta = document.querySelector<HTMLMetaElement>('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', COLORS[resolved]);
  return resolved;
}

export function saveTheme(theme: Theme): void {
  localStorage.setItem(THEME_KEY, theme);
}
