// Shared view plumbing: view contract, event-delegation actions and skeleton helper.

import { esc } from './format';

export type Cleanup = () => void;

/** One rendered view: HTML body plus optional post-mount hook. */
export interface View {
  html: string;
  mounted?: (root: HTMLElement) => void;
  /** Re-fetch the view's data region after an SSE event. Must not rebuild the
   *  whole view HTML: composer input and scroll stay intact. */
  refresh?: (root: HTMLElement) => void;
  /** Raw SSE hook for views that stream content (task detail timeline). */
  onEvent?: (ev: { name: string; data: unknown }) => void;
  /** Daemon reachability changed (false = submit must not pretend to work). */
  setOnline?: (online: boolean) => void;
  cleanup?: Cleanup;
}

/** Delegated action context passed to the app-level click handler. */
export interface ActionCtx {
  go(tab: string): void;
  openSession(id: string): void;
  openFile(path: string): void;
  decide(id: string, decision: string): void;
  logout(): void;
  refresh(): void;
}

export function skeleton(rows = 3): string {
  return Array.from({ length: rows }, () => '<div class="skel"></div>').join('');
}

export function emptyState(title: string, hint = ''): string {
  return `<div class="empty"><div class="empty-title">${esc(title)}</div>${
    hint ? `<div class="empty-hint">${esc(hint)}</div>` : ''
  }</div>`;
}

export function errorState(err: unknown): string {
  const msg = err instanceof Error ? err.message : String(err);
  return `<div class="empty error">加载失败：${esc(msg)}</div>`;
}

/** Badge element (small chip) with escaped label. */
export function chip(label: string, cls = ''): string {
  return `<span class="chip ${cls}">${esc(label)}</span>`;
}
