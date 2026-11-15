// Conversation search dialog: searches message content (not just titles) and
// shows the owning session plus a snippet around the match, like a chat app.

import type { ApiClient } from './api';
import { esc } from './format';
import type { SearchHit } from './types';

const CHAT_ICON =
  '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 5.5h16v10.5H9.5L5.5 20v-4H4V5.5z"/><path d="M8 9.5h8M8 13h5"/></svg>';

export interface SearchDialog {
  open(): void;
  close(): void;
  isOpen(): boolean;
}

/** Escape then highlight the query inside an already-escaped snippet. */
export function highlightSnippet(snippet: string, query: string): string {
  const safe = esc(snippet);
  const q = query.trim();
  if (!q) return safe;
  const needle = esc(q).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  return safe.replace(new RegExp(needle, 'gi'), (m) => `<mark>${m}</mark>`);
}

/** M/D date badge (the year is implicit for recent chats). */
function fmtDate(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return `${d.getMonth() + 1}/${d.getDate()}`;
}

export function createSearchDialog(
  root: HTMLElement,
  api: ApiClient,
  onOpenSession: (id: string) => void,
): SearchDialog {
  const overlay = root.querySelector<HTMLElement>('#search-overlay');
  const input = root.querySelector<HTMLInputElement>('#search-input');
  const results = root.querySelector<HTMLElement>('#search-results');
  const closeBtn = root.querySelector<HTMLButtonElement>('#search-close');
  let debounce: number | null = null;
  let seq = 0;

  const hint = (text = '输入关键词，搜索所有对话内容') => {
    if (results) results.innerHTML = `<div class="search-empty">${esc(text)}</div>`;
  };

  const renderHits = (hits: SearchHit[], query: string) => {
    if (!results) return;
    if (!hits.length) {
      results.innerHTML = `<div class="search-empty">没有匹配「${esc(query)}」的对话内容</div>`;
      return;
    }
    results.innerHTML = hits
      .map(
        (h) => `
        <button class="search-hit" type="button" data-search-session="${esc(h.session_id)}">
          <span class="search-hit-icon" aria-hidden="true">${CHAT_ICON}</span>
          <span class="search-hit-main">
            <span class="search-hit-head">
              <span class="search-hit-title">${esc(h.title || h.session_id.slice(0, 8))}</span>
              <span class="search-hit-date">${fmtDate(h.updated_at)}</span>
            </span>
            <span class="search-hit-snippet">${highlightSnippet(h.snippet, query)}</span>
          </span>
        </button>`,
      )
      .join('');
  };

  const close = () => {
    if (overlay) overlay.hidden = true;
    if (debounce !== null) window.clearTimeout(debounce);
    debounce = null;
    seq += 1; // drop in-flight responses
  };

  const open = () => {
    if (!overlay || !input) return;
    overlay.hidden = false;
    input.value = '';
    hint();
    input.focus();
  };

  input?.addEventListener('input', () => {
    if (debounce !== null) window.clearTimeout(debounce);
    const query = input.value.trim();
    seq += 1;
    const mine = seq;
    if (!query) {
      hint();
      return;
    }
    if (results) results.innerHTML = '<div class="search-empty">搜索中…</div>';
    debounce = window.setTimeout(() => {
      debounce = null;
      void (async () => {
        try {
          const hits = await api.search(query);
          if (mine === seq) renderHits(hits, query);
        } catch (err) {
          if (mine === seq && results) {
            results.innerHTML = `<div class="search-empty error">搜索失败：${esc(
              err instanceof Error ? err.message : String(err),
            )}</div>`;
          }
        }
      })();
    }, 300);
  });

  closeBtn?.addEventListener('click', close);
  overlay?.addEventListener('click', (e) => {
    if (e.target === overlay) close();
  });
  overlay?.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') close();
  });
  results?.addEventListener('click', (e) => {
    const hit = (e.target as HTMLElement).closest<HTMLElement>('[data-search-session]');
    if (!hit?.dataset.searchSession) return;
    close();
    onOpenSession(hit.dataset.searchSession);
  });

  return { open, close, isOpen: () => Boolean(overlay && !overlay.hidden) };
}
