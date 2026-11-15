// @vitest-environment happy-dom
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from './api';
import type { SearchHit } from './types';
import { createSearchDialog, highlightSnippet } from './search';

function hit(over: Partial<SearchHit> = {}): SearchHit {
  return {
    session_id: 's1',
    title: '网络分析',
    role: 'assistant',
    snippet: '…用于防火墙策略或网络分析…',
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-02T00:00:00Z',
    ...over,
  };
}

function mountDialog(hits: SearchHit[]) {
  const root = document.createElement('div');
  root.innerHTML = `
    <div id="search-overlay" hidden>
      <input id="search-input" />
      <button id="search-close" type="button"></button>
      <div id="search-results"></div>
    </div>`;
  document.body.append(root);
  const api = { search: vi.fn(async () => hits) } as unknown as ApiClient;
  const onOpen = vi.fn();
  const dialog = createSearchDialog(root, api, onOpen);
  return { root, api, onOpen, dialog };
}

describe('highlightSnippet', () => {
  it('escapes markup and wraps the query', () => {
    expect(highlightSnippet('<b>防火墙</b>', '防火墙')).toBe(
      '&lt;b&gt;<mark>防火墙</mark>&lt;/b&gt;',
    );
  });

  it('matches case-insensitively and handles regex characters', () => {
    expect(highlightSnippet('Use FTS5 (search)', 'fts5')).toContain('<mark>FTS5</mark>');
    expect(highlightSnippet('a+b', 'a+b')).toContain('<mark>a+b</mark>');
  });
});

describe('search dialog', () => {
  it('opens with a hint and closes on Escape', () => {
    const { root, dialog } = mountDialog([]);
    const overlay = root.querySelector<HTMLElement>('#search-overlay')!;
    dialog.open();
    expect(overlay.hidden).toBe(false);
    expect(root.querySelector('#search-results')?.textContent).toContain('搜索所有对话内容');
    overlay.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    expect(overlay.hidden).toBe(true);
  });

  it('searches message content, highlights the term and opens the session', async () => {
    const { root, onOpen, dialog } = mountDialog([hit()]);
    dialog.open();
    const input = root.querySelector<HTMLInputElement>('#search-input')!;
    input.value = '防火墙';
    input.dispatchEvent(new Event('input'));
    await vi.waitFor(() => {
      expect(root.querySelector('#search-results')?.textContent).toContain('网络分析');
    });
    const snippet = root.querySelector('.search-hit-snippet')!;
    expect(snippet.querySelector('mark')?.textContent).toBe('防火墙');
    root.querySelector<HTMLElement>('[data-search-session="s1"]')!.click();
    expect(onOpen).toHaveBeenCalledWith('s1');
    expect(root.querySelector<HTMLElement>('#search-overlay')!.hidden).toBe(true);
  });

  it('shows an empty message when nothing matches', async () => {
    const { root, dialog } = mountDialog([]);
    dialog.open();
    const input = root.querySelector<HTMLInputElement>('#search-input')!;
    input.value = '不存在';
    input.dispatchEvent(new Event('input'));
    await vi.waitFor(() => {
      expect(root.querySelector('#search-results')?.textContent).toContain('没有匹配');
    });
  });
});
