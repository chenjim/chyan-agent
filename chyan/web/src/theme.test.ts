// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { applyTheme, loadTheme, resolveTheme, saveTheme, THEME_KEY } from './theme';

describe('theme preference', () => {
  beforeEach(() => {
    localStorage.clear();
    document.documentElement.removeAttribute('data-theme');
    document.querySelector('meta[name="theme-color"]')?.remove();
  });

  it('defaults to system and rejects unknown values', () => {
    expect(loadTheme()).toBe('system');
    localStorage.setItem(THEME_KEY, 'bogus');
    expect(loadTheme()).toBe('system');
    localStorage.setItem(THEME_KEY, 'dark');
    expect(loadTheme()).toBe('dark');
  });

  it('resolves explicit themes and follows the OS for system', () => {
    expect(resolveTheme('light', true)).toBe('light');
    expect(resolveTheme('dark', false)).toBe('dark');
    expect(resolveTheme('system', true)).toBe('dark');
    expect(resolveTheme('system', false)).toBe('light');
  });

  it('applies the resolved theme and keeps the chrome colour in sync', () => {
    const meta = document.createElement('meta');
    meta.name = 'theme-color';
    document.head.append(meta);
    vi.stubGlobal('matchMedia', () => ({ matches: false }));
    applyTheme('light');
    expect(document.documentElement.dataset.theme).toBe('light');
    expect(meta.content).toBe('#f7f7f8');
    vi.stubGlobal('matchMedia', () => ({ matches: true }));
    expect(applyTheme('system')).toBe('dark');
    expect(document.documentElement.dataset.theme).toBe('dark');
    expect(meta.content).toBe('#18181a');
    vi.unstubAllGlobals();
  });

  it('persists the preference', () => {
    saveTheme('dark');
    expect(localStorage.getItem(THEME_KEY)).toBe('dark');
  });
});
