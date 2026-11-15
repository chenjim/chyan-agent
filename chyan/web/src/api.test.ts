// Tests for the REST client (fetch mocked, no network).

import { describe, expect, it, vi } from 'vitest';
import { ApiClient, ApiError } from './api';

function mockFetch(handler: (url: string, init: RequestInit) => Response | Promise<Response>) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) =>
    handler(String(input), init ?? {}),
  ) as unknown as typeof fetch;
}

function jsonRes(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('ApiClient', () => {
  it('sends Bearer token and parses task list', async () => {
    let seenAuth = '';
    const api = new ApiClient({
      token: 'tok123',
      fetchImpl: mockFetch((url, init) => {
        expect(url).toBe('/api/v1/tasks');
        seenAuth = (init.headers as Record<string, string>).Authorization;
        return jsonRes([{ id: 't1', session_id: 's1', status: 'running', created_at: '', updated_at: '' }]);
      }),
    });
    const tasks = await api.listTasks();
    expect(tasks).toHaveLength(1);
    expect(seenAuth).toBe('Bearer tok123');
  });

  it('creates task with JSON body and content-type', async () => {
    const api = new ApiClient({
      token: 't',
      fetchImpl: mockFetch((url, init) => {
        expect(url).toBe('/api/v1/tasks');
        expect(init.method).toBe('POST');
        expect((init.headers as Record<string, string>)['Content-Type']).toBe('application/json');
        expect(JSON.parse(String(init.body))).toEqual({ prompt: 'hello', agent: 'explorer' });
        return jsonRes({ id: 't1', session_id: 's1', status: 'pending', created_at: '', updated_at: '' });
      }),
    });
    await api.createTask({ prompt: 'hello', agent: 'explorer' });
  });

  it('sends image data URLs with a task', async () => {
    const api = new ApiClient({
      token: 't',
      fetchImpl: mockFetch((_url, init) => {
        expect(JSON.parse(String(init.body))).toEqual({
          prompt: 'look',
          images: ['data:image/png;base64,AAAA'],
        });
        return jsonRes({ id: 't1', session_id: 's1', status: 'pending', created_at: '', updated_at: '' });
      }),
    });
    await api.createTask({ prompt: 'look', images: ['data:image/png;base64,AAAA'] });
  });

  it('cancels a task by id', async () => {
    const api = new ApiClient({
      token: 't',
      fetchImpl: mockFetch((url, init) => {
        expect(url).toBe('/api/v1/tasks/tsk1/cancel');
        expect(init.method).toBe('POST');
        return jsonRes({ id: 'tsk1', session_id: 's1', status: 'interrupted', created_at: '', updated_at: '' });
      }),
    });
    const task = await api.cancelTask('tsk1');
    expect(task.status).toBe('interrupted');
  });

  it('encodes ids and query params', async () => {    const urls: string[] = [];
    const api = new ApiClient({
      token: 't',
      fetchImpl: mockFetch((url) => {
        urls.push(url);
        return jsonRes([]);
      }),
    });
    await api.readFile('a b/中.txt');
    await api.decideApproval('id/1', { decision: 'approve' });
    expect(urls[0]).toBe('/api/v1/files/content?path=a+b%2F%E4%B8%AD.txt');
    expect(urls[1]).toBe('/api/v1/approvals/id%2F1');
  });

  it('omits empty query params', async () => {
    const api = new ApiClient({
      token: 't',
      fetchImpl: mockFetch((url) => {
        expect(url).toBe('/api/v1/files');
        return jsonRes([]);
      }),
    });
    await api.listFiles('');
  });

  it('throws ApiError with server error message', async () => {
    const api = new ApiClient({
      token: 't',
      fetchImpl: mockFetch(() => jsonRes({ error: 'nope' }, 401)),
    });
    const err = await api.status().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(401);
    expect((err as ApiError).message).toBe('nope');
  });

  it('falls back to status code for non-JSON error body', async () => {
    const api = new ApiClient({
      token: 't',
      fetchImpl: mockFetch(() => new Response('bad gateway', { status: 502 })),
    });
    const err = await api.listSessions().catch((e: unknown) => e);
    expect((err as ApiError).message).toBe('502');
  });

  it('fetches session detail and messages', async () => {
    const api = new ApiClient({
      token: 't',
      fetchImpl: mockFetch((url) => {
        if (url.endsWith('/messages')) return jsonRes([{ id: 'm1', role: 'user', text: 'hi', created_at: '' }]);
        return jsonRes({ id: 's 1', created_at: '', updated_at: '', messages: [] });
      }),
    });
    const s = await api.getSession('s 1');
    expect(s.id).toBe('s 1');
    const msgs = await api.sessionMessages('s 1');
    expect(msgs[0].role).toBe('user');
  });
});

describe('ApiClient management endpoints', () => {
  it('renames, deletes and exports a session', async () => {
    const calls: { url: string; method?: string; body?: string }[] = [];
    const api = new ApiClient({
      token: 't',
      fetchImpl: mockFetch((url, init) => {
        calls.push({ url, method: init.method, body: init.body ? String(init.body) : undefined });
        if (url.endsWith('/export')) {
          return new Response('{"format_version":1}', {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          });
        }
        return jsonRes({ id: 's1', title: 'x', created_at: '', updated_at: '' });
      }),
    });
    await api.renameSession('s1', '新标题');
    expect(calls[0]).toEqual({ url: '/api/v1/sessions/s1', method: 'PATCH', body: '{"title":"新标题"}' });
    await api.deleteSession('s1');
    expect(calls[1]).toMatchObject({ url: '/api/v1/sessions/s1', method: 'DELETE' });
    const blob = await api.exportSession('s1');
    expect(blob.size).toBeGreaterThan(0);
    expect(calls[2].url).toBe('/api/v1/sessions/s1/export');
  });

  it('downloads files and passes the artifact source', async () => {
    const api = new ApiClient({
      token: 't',
      fetchImpl: mockFetch((url) => {
        expect(url).toContain('source=artifacts');
        return new Response('abc', { status: 200 });
      }),
    });
    const blob = await api.downloadFile('/abs/out.txt', '', 'artifacts');
    expect(blob.size).toBe(3);
  });

  it('manages cron jobs', async () => {
    const calls: { url: string; method?: string; body?: string }[] = [];
    const job = { id: 'j1', name: 'n', schedule: '@every 1h', prompt: 'p', enabled: true };
    const api = new ApiClient({
      token: 't',
      fetchImpl: mockFetch((url, init) => {
        calls.push({ url, method: init.method, body: init.body ? String(init.body) : undefined });
        if (init.method === 'DELETE') return new Response(null, { status: 204 });
        if (url.endsWith('/run')) return jsonRes({ started: true }, 202);
        if (!init.method || init.method === 'GET') return jsonRes([job]);
        return jsonRes(job);
      }),
    });
    const jobs = await api.listCron();
    expect(jobs[0].id).toBe('j1');
    await api.createCron({ name: 'n', schedule: '@every 1h', prompt: 'p', enabled: true });
    await api.updateCron('j1', { name: 'n2', schedule: '@every 1h', prompt: 'p', enabled: false });
    await api.runCron('j1');
    await api.deleteCron('j1');
    expect(calls.map((c) => c.method ?? 'GET')).toEqual(['GET', 'POST', 'PATCH', 'POST', 'DELETE']);
    expect(calls[2].body).toContain('"enabled":false');
  });

  it('queries memory and runs the dream pass', async () => {
    const urls: string[] = [];
    const api = new ApiClient({
      token: 't',
      fetchImpl: mockFetch((url, init) => {
        urls.push(url);
        if (url.includes('/supersede')) return jsonRes({ id: 'm1', status: 'superseded' });
        if (init.method === 'POST') return jsonRes({ Examined: 1 });
        return jsonRes([{ id: 'm1', name: 'n' }]);
      }),
    });
    await api.listMemory({ q: '部署 目录', domain: 'global', kind: 'fact', project: '/repo' });
    await api.supersedeMemory('m1', '/repo');
    await api.supersedeMemory('m1');
    await api.getDream();
    await api.runDream();
    expect(urls[0]).toBe(
      '/api/v1/memory?' + new URLSearchParams({ q: '部署 目录', domain: 'global', kind: 'fact', project: '/repo' }),
    );
    expect(urls[1]).toContain('/memory/m1/supersede?project=%2Frepo');
    expect(urls[2]).toBe('/api/v1/memory/m1/supersede');
  });

  it('fetches a task and the workspace info', async () => {
    const api = new ApiClient({
      token: 't',
      fetchImpl: mockFetch((url) => jsonRes(url.includes('workspace') ? { path: '/w', recent: [] } : { id: 't1' })),
    });
    expect((await api.getTask('t1')).id).toBe('t1');
    expect((await api.getWorkspace()).path).toBe('/w');
  });
});
