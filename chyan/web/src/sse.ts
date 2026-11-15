// SSE subscription for GET /api/v1/events using fetch streaming (EventSource
// cannot send the Authorization header; the daemon contract requires Bearer auth).

import type { SseEvent } from './state';

export interface SseHandle {
  close(): void;
}

export interface SseOptions {
  token: string;
  onEvent(ev: SseEvent): void;
  onError?(err: unknown): void;
  /** Called once when the stream ends without close() (drop or server stop). */
  onClose?(): void;
  url?: string; // default '/api/v1/events'
  fetchImpl?: typeof fetch;
}

const NAME_RE = /^event:\s*(.+)$/;

/**
 * Parse an SSE chunk buffer into events plus the unparsed remainder.
 * Supports `event:`, `data:` and blank-line dispatch; `id:`/`retry:` are ignored.
 */
export function parseSseChunk(buf: string): { events: SseEvent[]; rest: string } {
  const events: SseEvent[] = [];
  const parts = buf.split(/\r?\n\r?\n/);
  const rest = parts.pop() ?? '';
  for (const block of parts) {
    let name = 'message';
    const dataLines: string[] = [];
    for (const line of block.split(/\r?\n/)) {
      const m = NAME_RE.exec(line);
      if (m) {
        name = m[1].trim();
        continue;
      }
      if (line.startsWith('data:')) dataLines.push(line.slice(5).replace(/^ /, ''));
    }
    const raw = dataLines.join('\n');
    let data: unknown = raw;
    try {
      data = JSON.parse(raw);
    } catch {
      // keep raw string when payload is not JSON
    }
    events.push({ name, data });
  }
  return { events, rest };
}

/** Open the SSE stream; returns a handle whose close() aborts the connection. */
export function openSse(opts: SseOptions): SseHandle {
  const url = opts.url ?? '/api/v1/events';
  const fetchImpl = opts.fetchImpl ?? fetch.bind(globalThis);
  const ctrl = new AbortController();
  void (async () => {
    try {
      const res = await fetchImpl(url, {
        headers: {
          Authorization: `Bearer ${opts.token}`,
          Accept: 'text/event-stream',
        },
        signal: ctrl.signal,
      });
      if (!res.ok || !res.body) throw new Error(`SSE HTTP ${res.status}`);
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        const { events, rest } = parseSseChunk(buf);
        buf = rest;
        for (const ev of events) opts.onEvent(ev);
      }
      // Stream ended without an error (daemon restart, proxy idle timeout):
      // the caller decides whether to resubscribe.
      if (!ctrl.signal.aborted) opts.onClose?.();
    } catch (err) {
      if (ctrl.signal.aborted) return;
      opts.onError?.(err);
      opts.onClose?.();
    }
  })();
  return { close: () => ctrl.abort() };
}
