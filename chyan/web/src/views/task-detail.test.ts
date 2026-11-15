// @vitest-environment happy-dom
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api';
import type { ApprovalTicket, Message, Session, Task } from '../types';
import { renderText, taskDetailView } from './task-detail';

function task(over: Partial<Task> = {}): Task {
  return {
    id: 't1',
    session_id: 's1',
    status: 'running',
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

function message(over: Partial<Message> = {}): Message {
  return {
    id: 'm1',
    role: 'assistant',
    text: 'hi',
    created_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

function fakeApi(over: Partial<ApiClient> = {}): ApiClient {
  const messages: Message[] = [
    message({ id: 'u1', role: 'user', text: 'do the thing' }),
    message({ id: 'a1', role: 'assistant', text: 'running it', tool_calls: ['bash'] }),
    message({
      id: 't1',
      role: 'tool',
      text: 'x'.repeat(500) + '\n[artifact: artifacts/s1/bash/out.txt]',
    }),
  ];
  return {
    getTask: vi.fn(async () => task()),
    getSession: vi.fn(async (): Promise<Session> => ({
      id: 's1',
      created_at: '2024-01-01T00:00:00Z',
      updated_at: '2024-01-01T00:00:00Z',
      messages,
    })),
    listApprovals: vi.fn(async (): Promise<ApprovalTicket[]> => [{
      id: 'ap1', session_id: 's1', action: 'bash', resource: 'ls',
      diff: '--- a\n+++ b', created_at: '2024-01-01T00:00:00Z',
    }]),
    decideApproval: vi.fn(async () => ({ id: 'ap1', action: 'bash', resource: 'ls', created_at: '' })),
    cancelTask: vi.fn(async () => task({ status: 'interrupted' })),
    ...over,
  } as unknown as ApiClient;
}

async function mount(root: HTMLElement, api: ApiClient, options = {}) {
  const view = taskDetailView(api, 't1', options);
  root.innerHTML = view.html;
  view.mounted?.(root);
  await vi.waitFor(() => {
    expect(root.querySelector('#task-timeline')?.textContent).toContain('do the thing');
  });
  return view;
}

describe('renderText', () => {
  it('links artifact markers and escapes other text', () => {
    const html = renderText('[artifact: a&b.txt] <danger>');
    expect(html).toContain('#/artifacts/a%26b.txt');
    expect(html).toContain('&lt;danger&gt;');
  });
});

describe('taskDetailView', () => {
  it('renders the transcript, folds long tool output and links artifacts', async () => {
    const root = document.createElement('div');
    await mount(root, fakeApi());
    expect(root.querySelector('#task-timeline')?.textContent).toContain('running it');
    expect(root.querySelector('details.tool-output')).not.toBeNull();
    const link = root.querySelector<HTMLAnchorElement>('.artifact-link');
    expect(link?.getAttribute('href')).toContain('#/artifacts/artifacts%2Fs1%2Fbash%2Fout.txt');
  });

  it('appends live output and progress from SSE', async () => {
    const root = document.createElement('div');
    const view = await mount(root, fakeApi());
    view.onEvent?.({ name: 'task.output', data: { id: 't1', text: 'LIVE-TEXT' } });
    view.onEvent?.({ name: 'task.progress', data: { id: 't1', kind: 'tool_run', tool: 'bash', detail: 'ls' } });
    await vi.waitFor(() => {
      expect(root.querySelector('#task-timeline')?.textContent).toContain('LIVE-TEXT');
      expect(root.querySelector('.tool-progress')?.textContent).toContain('bash');
    });
  });

  it('resolves approvals and cancels the task', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    await mount(root, api);
    root.querySelector<HTMLButtonElement>('[data-decision="approve"]')?.click();
    await vi.waitFor(() => expect(api.decideApproval).toHaveBeenCalledWith('ap1', { decision: 'approve', reason: '' }));
    root.querySelector<HTMLButtonElement>('[data-cancel-task]')?.click();
    await vi.waitFor(() => expect(api.cancelTask).toHaveBeenCalledWith('t1'));
  });

  it('opens the session and navigates back', async () => {
    const onOpenSession = vi.fn();
    const onBack = vi.fn();
    const root = document.createElement('div');
    await mount(root, fakeApi(), { onOpenSession, onBack });
    root.querySelector<HTMLButtonElement>('#task-open-session')?.click();
    expect(onOpenSession).toHaveBeenCalledWith('s1');
    root.querySelector<HTMLButtonElement>('#task-back')?.click();
    expect(onBack).toHaveBeenCalled();
  });
});

describe('taskDetailView edge paths', () => {
  it('shows a failed task without a transcript and folds approvals without diff', async () => {
    const api = fakeApi({
      getTask: vi.fn(async () => task({ status: 'failed', err: '<boom>', reply: 'partial' })),
      getSession: vi.fn(async () => Promise.reject(new Error('gone'))),
      listApprovals: vi.fn(async () => [
        { id: 'ap2', session_id: 's1', action: 'edit', resource: 'a.go', created_at: '' },
        { id: 'ap3', session_id: 'other', action: 'bash', resource: 'ls', created_at: '' },
      ]),
    });
    const root = document.createElement('div');
    const view = taskDetailView(api, 't1');
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#task-timeline')?.textContent).toContain('<boom>'));
    expect(root.querySelectorAll('[data-approval]')).toHaveLength(1);
    expect(root.querySelector('.diff')).toBeNull();
    expect(root.querySelector('#task-timeline')?.textContent).toContain('partial');
  });

  it('renders tool progress variants and ignores unrelated events', async () => {
    const root = document.createElement('div');
    const view = await mount(root, fakeApi());
    view.onEvent?.({ name: 'task.progress', data: { id: 'other', kind: 'tool_run', tool: 'x' } });
    view.onEvent?.({ name: 'task.progress', data: { id: 't1', kind: 'thinking', detail: '规划' } });
    view.onEvent?.({ name: 'task.progress', data: { id: 't1', kind: 'tool_done', tool: 'bash', elapsed_ms: 12, err: '<bad>' } });
    view.onEvent?.({ name: 'task.progress', data: { id: 't1', kind: 'tool_done', tool: 'read' } });
    await vi.waitFor(() => {
      expect(root.querySelector('.tool-progress')?.textContent).toContain('规划');
      expect(root.querySelector('.tool-progress')?.textContent).toContain('<bad>');
      expect(root.querySelector('.tool-progress')?.textContent).toContain('read');
      expect(root.querySelector('#task-timeline')?.textContent).not.toContain('other');
    });
  });

  it('clears live output when a run ends with an error', async () => {
    let current = task({ status: 'running' });
    const api = fakeApi({
      getTask: vi.fn(async () => current),
    });
    const root = document.createElement('div');
    const view = await mount(root, api);
    view.onEvent?.({ name: 'task.output', data: { id: 't1', text: 'LIVE' } });
    await vi.waitFor(() => expect(root.querySelector('#task-timeline')?.textContent).toContain('LIVE'));
    current = task({ status: 'failed', err: 'boom' });
    view.onEvent?.({ name: 'task.done', data: { id: 't1', status: 'failed', err: 'boom' } });
    await vi.waitFor(() => {
      expect(root.querySelector('#task-timeline')?.textContent).not.toContain('LIVE');
      expect(root.querySelector('#task-timeline')?.textContent).toContain('boom');
    });
  });

  it('reports cancel and approval failures', async () => {
    const api = fakeApi({
      cancelTask: vi.fn(async () => Promise.reject(new Error('cannot cancel'))),
      decideApproval: vi.fn(async () => Promise.reject(new Error('denied'))),
    });
    const root = document.createElement('div');
    await mount(root, api);
    root.querySelector<HTMLButtonElement>('[data-cancel-task]')?.click();
    await vi.waitFor(() =>
      expect(root.querySelector('#task-detail-msg')?.textContent).toContain('取消失败：cannot cancel'),
    );
    root.querySelector<HTMLButtonElement>('[data-decision="deny"]')?.click();
    await vi.waitFor(() =>
      expect(root.querySelector('#task-detail-msg')?.textContent).toContain('审批失败：denied'),
    );
  });
});
