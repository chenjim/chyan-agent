// Tests for SSE stream parsing and connection handling (fetch mocked).

import { describe, expect, it, vi } from 'vitest';
import { openSse, parseSseChunk } from './sse';

describe('parseSseChunk', () => {
  it('parses named events with JSON payloads', () => {
    const { events, rest } = parseSseChunk('event: task.started\ndata: {"id":"t1"}\n\n');
    expect(events).toEqual([{ name: 'task.started', data: { id: 't1' } }]);
    expect(rest).toBe('');
  });

  it('keeps partial block in the remainder', () => {
    const { events, rest } = parseSseChunk('event: task.output\ndata: {"id":"t1"}\n\nevent: task.d');
    expect(events).toHaveLength(1);
    expect(rest).toBe('event: task.d');
  });

  it('joins multi-line data and keeps raw text for non-JSON', () => {
    const { events } = parseSseChunk('event: note\ndata: line1\ndata: line2\n\n');
    expect(events[0]).toEqual({ name: 'note', data: 'line1\nline2' });
  });

  it('defaults to message event name and strips one space after data:', () => {
    const { events } = parseSseChunk('data: hello\n\n');
    expect(events[0].name).toBe('message');
    expect(events[0].data).toBe('hello');
  });

  it('handles CRLF separators', () => {
    const { events } = parseSseChunk('event: task.done\r\ndata: {"id":"t"}\r\n\r\n');
    expect(events[0].name).toBe('task.done');
    expect(events[0].data).toEqual({ id: 't' });
  });
});

describe('openSse', () => {
  it('streams events with auth header and stops on close', async () => {
    const received: string[] = [];
    const ctrl = new AbortController();
    const stream = new ReadableStream<Uint8Array>({
      start(c) {
        const enc = new TextEncoder();
        c.enqueue(enc.encode('event: approval.requested\ndata: {"id":"a1"}\n\n'));
        c.enqueue(enc.encode('event: task.done\ndata: {"id":"t1"}\n\n'));
        // keep open until aborted
        ctrl.signal.addEventListener('abort', () => c.close());
      },
    });
    let seenAuth = '';
    const fetchImpl = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      seenAuth = (init?.headers as Record<string, string>).Authorization;
      return new Response(stream, { status: 200 });
    }) as unknown as typeof fetch;

    const handle = openSse({
      token: 'tok',
      fetchImpl,
      onEvent: (ev) => received.push(ev.name),
    });
    await vi.waitFor(() => expect(received).toHaveLength(2));
    expect(seenAuth).toBe('Bearer tok');
    handle.close();
  });

  it('reports HTTP errors via onError', async () => {
    const onError = vi.fn();
    const fetchImpl = (async () => new Response('', { status: 503 })) as unknown as typeof fetch;
    openSse({ token: 'tok', fetchImpl, onEvent: () => {}, onError });
    await vi.waitFor(() => expect(onError).toHaveBeenCalled());
  });
});

describe('openSse reconnect hook', () => {
  it('calls onClose when the stream ends without close()', async () => {
    const onClose = vi.fn();
    const fetchImpl = (async () =>
      new Response(new ReadableStream<Uint8Array>({ start: (c) => c.close() }), {
        status: 200,
      })) as unknown as typeof fetch;
    openSse({ token: 'tok', fetchImpl, onEvent: () => {}, onClose });
    await vi.waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  });

  it('calls onClose after an error but not after close()', async () => {
    const onClose = vi.fn();
    const onError = vi.fn();
    const fetchImpl = (async () => new Response('', { status: 503 })) as unknown as typeof fetch;
    const handle = openSse({ token: 'tok', fetchImpl, onEvent: () => {}, onError, onClose });
    await vi.waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    handle.close();

    // A handled close must not schedule another reconnect.
    const ctrlClosed = vi.fn();
    const hanging = new ReadableStream<Uint8Array>({ start() {} });
    const h2 = openSse({
      token: 'tok',
      fetchImpl: (async () => new Response(hanging, { status: 200 })) as unknown as typeof fetch,
      onEvent: () => {},
      onClose: ctrlClosed,
    });
    h2.close();
    await new Promise((r) => setTimeout(r, 20));
    expect(ctrlClosed).not.toHaveBeenCalled();
  });
});
