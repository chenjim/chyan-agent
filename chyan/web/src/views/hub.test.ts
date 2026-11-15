// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, type ApiClient } from '../api';
import type { ApprovalTicket, Message, Session, SessionSummary, Task } from '../types';
import { hubView, legacyCopy, readDataURL } from './hub';

function session(over: Partial<SessionSummary> = {}): SessionSummary {
  return {
    id: 's1',
    title: '有任务的会话',
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-02T00:00:00Z',
    ...over,
  };
}

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
    role: 'user',
    text: 'hello',
    created_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

function fakeApi(over: Partial<ApiClient> = {}): ApiClient {
  return {
    listSessions: vi.fn(async () => [
      session({ id: 's1', title: '有任务的会话' }),
      session({ id: 's2', title: '空会话', updated_at: '2024-01-03T00:00:00Z' }),
    ]),
    listTasks: vi.fn(async () => [task()]),
    getSession: vi.fn(async (id: string): Promise<Session> => ({
      ...session({ id, title: id }),
      messages: [message()],
    })),
    // 页头显示当前窗口占用；这里给未定义，走"没有预算就不显示"的分支。
    sessionContext: vi.fn(async () => undefined),
    renameSession: vi.fn(async () => session()),
    deleteSession: vi.fn(async () => undefined),
    exportSession: vi.fn(async () => new Blob(['{}'])),
    createTask: vi.fn(async () => task({ id: 'new', session_id: 's-new', status: 'pending' })),
    cancelTask: vi.fn(async () => undefined),
    listApprovals: vi.fn(async (): Promise<ApprovalTicket[]> => []),
    decideApproval: vi.fn(async () => undefined),
    downloadFile: vi.fn(async () => new Blob([])),
    getWorkspace: vi.fn(async () => ({ path: '/repo', recent: ['/repo'] })),
    ...over,
  } as unknown as ApiClient;
}

async function mount(root: HTMLElement, api: ApiClient, options: Parameters<typeof hubView>[1] = {}) {
  const view = hubView(api, options);
  root.innerHTML = view.html;
  view.mounted?.(root);
  await vi.waitFor(() => {
    expect(root.querySelector('#hub-rows')?.textContent).toContain('有任务的会话');
  });
  return view;
}

beforeEach(() => {
  localStorage.clear();
  document.body.innerHTML = '';
});

describe('hub list', () => {
  it('merges sessions with and without tasks into one list', async () => {
    const root = document.createElement('div');
    await mount(root, fakeApi());
    const rows = root.querySelector('#hub-rows')!;
    expect(rows.textContent).toContain('有任务的会话');
    expect(rows.textContent).toContain('空会话');
    expect(rows.innerHTML).toContain('data-session="s1"');
    // The latest task state rides on the session row.
    expect(rows.innerHTML).toContain('运行中');
  });

  it('filters by search text and task status', async () => {
    const root = document.createElement('div');
    await mount(root, fakeApi());
    const search = root.querySelector<HTMLInputElement>('#hub-search')!;
    search.value = '空会话';
    search.dispatchEvent(new Event('input'));
    expect(root.querySelector('#hub-rows')?.textContent).not.toContain('有任务的会话');

    search.value = '';
    search.dispatchEvent(new Event('input'));
    const status = root.querySelector<HTMLSelectElement>('#task-filter')!;
    status.value = 'running';
    status.dispatchEvent(new Event('change'));
    expect(root.querySelector('#hub-rows')?.textContent).toContain('有任务的会话');
    expect(root.querySelector('#hub-rows')?.textContent).not.toContain('空会话');
  });

  it('shows an empty state when no sessions exist', async () => {
    const root = document.createElement('div');
    const api = fakeApi({ listSessions: vi.fn(async () => []) });
    const view = hubView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => {
      expect(root.querySelector('#hub-rows')?.textContent).toContain('还没有会话');
    });
  });

    it('opens a session and the task timeline from their actions', async () => {
    const onOpenSession = vi.fn();
    const onOpenTask = vi.fn();
    const root = document.createElement('div');
    await mount(root, fakeApi(), { onOpenSession, onOpenTask });
    root.querySelector<HTMLElement>('[data-timeline="t1"]')!.click();
    expect(onOpenTask).toHaveBeenCalledWith('t1');
    root.querySelector<HTMLElement>('[data-session="s1"]')!.click();
    expect(onOpenSession).toHaveBeenCalledWith('s1');
  });

  it('cancels a running task from its row', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    await mount(root, api);
    root.querySelector<HTMLButtonElement>('[data-cancel="t1"]')!.click();
    await vi.waitFor(() => expect(api.cancelTask).toHaveBeenCalledWith('t1'));
  });
});

describe('hub composer', () => {
  it('submits a trimmed prompt and opens the created session', async () => {
    const api = fakeApi();
    const onCreated = vi.fn();
    const root = document.createElement('div');
    await mount(root, api, { onCreated });
    root.querySelector<HTMLTextAreaElement>('#task-prompt')!.value = '  hello  ';
    root.querySelector<HTMLFormElement>('#task-form')!.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true }),
    );
    await vi.waitFor(() => expect(api.createTask).toHaveBeenCalledWith({ prompt: 'hello' }));
    expect(onCreated).toHaveBeenCalledWith('s-new');
    expect(localStorage.getItem('chyan_draft:task')).toBeNull();
  });

  it('sends with Enter and keeps Ctrl+Enter for a newline', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    await mount(root, api);
    const prompt = root.querySelector<HTMLTextAreaElement>('#task-prompt')!;
    prompt.value = 'send me';
    prompt.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await vi.waitFor(() => expect(api.createTask).toHaveBeenCalledWith({ prompt: 'send me' }));

    // Ctrl+Enter must stay a line break, not a submit.
    const api2 = fakeApi();
    const root2 = document.createElement('div');
    await mount(root2, api2);
    const prompt2 = root2.querySelector<HTMLTextAreaElement>('#task-prompt')!;
    prompt2.value = 'multi\nline';
    const event = new KeyboardEvent('keydown', {
      key: 'Enter',
      ctrlKey: true,
      bubbles: true,
      cancelable: true,
    });
    prompt2.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
    await Promise.resolve();
    expect(api2.createTask).not.toHaveBeenCalled();
  });

  it('does not submit while an IME composition is active', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    await mount(root, api);
    const prompt = root.querySelector<HTMLTextAreaElement>('#task-prompt')!;
    prompt.value = '中文输入';
    prompt.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', isComposing: true, bubbles: true }),
    );
    await Promise.resolve();
    expect(api.createTask).not.toHaveBeenCalled();
  });

  it('continues the preselected session and can clear it', async () => {
    const api = fakeApi();
    const onClearContinue = vi.fn();
    const root = document.createElement('div');
    await mount(root, api, { continueSessionId: 's1', onClearContinue });
    expect(root.querySelector('#composer-continue')?.textContent).toContain('有任务的会话');
    root.querySelector<HTMLTextAreaElement>('#task-prompt')!.value = 'again';
    root.querySelector<HTMLFormElement>('#task-form')!.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true }),
    );
    await vi.waitFor(() =>
      expect(api.createTask).toHaveBeenCalledWith({ prompt: 'again', session_id: 's1' }),
    );
    root.querySelector<HTMLButtonElement>('#continue-clear')!.click();
    expect(onClearContinue).toHaveBeenCalled();
  });

  it('accepts pasted images, previews them and opens the fullscreen viewer', async () => {
    const root = document.createElement('div');
    await mount(root, fakeApi());
    const file = new File([new Uint8Array([1, 2, 3])], 'shot.png', { type: 'image/png' });
    const event = new Event('paste', { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'clipboardData', {
      value: { items: [{ kind: 'file', type: 'image/png', getAsFile: () => file }] },
    });
    root.querySelector<HTMLFormElement>('#task-form')!.dispatchEvent(event);
    await vi.waitFor(() => expect(root.querySelector('.composer-image')).not.toBeNull());

    root.querySelector<HTMLImageElement>('.composer-image img')!.click();
    await vi.waitFor(() =>
      expect(root.querySelector<HTMLElement>('#image-viewer')!.hidden).toBe(false),
    );
    root.querySelector<HTMLButtonElement>('#image-viewer-close')!.click();
    expect(root.querySelector<HTMLElement>('#image-viewer')!.hidden).toBe(true);

    root.querySelector<HTMLButtonElement>('[data-remove-image]')!.click();
    expect(root.querySelector('.composer-image')).toBeNull();
    expect(root.querySelector<HTMLElement>('#composer-images')!.hidden).toBe(true);
  });

  it('disables submit while the daemon is offline', async () => {
    const root = document.createElement('div');
    const view = await mount(root, fakeApi());
    view.setOnline?.(false);
    expect(root.querySelector<HTMLButtonElement>('#task-submit')!.disabled).toBe(true);
    expect(root.querySelector<HTMLTextAreaElement>('#task-prompt')!.readOnly).toBe(true);
  });
});

describe('hub detail', () => {
  it('renders the transcript with an inline composer and its timeline', async () => {
    const root = document.createElement('div');
    const view = hubView(fakeApi(), { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#task-form')).not.toBeNull());
    expect(root.querySelector('.msg-list')?.textContent).toContain('hello');
    expect(root.querySelector('#session-timeline')).not.toBeNull();
  });

  it('opens the per-turn trace panel and attributes the slow phase', async () => {
    const traces = vi.fn(async () => ({
      turns: [
        {
          summary: {
            trace_id: 'tr2',
            span_count: 2,
            start: '2024-01-02T10:01:00Z',
            end: '2024-01-02T10:01:04Z',
            duration_ms: 4000,
            model_ms: 2500,
            tool_execution_ms: 1000,
            approval_wait_ms: 300,
            memory_ms: 0,
            subagent_ms: 0,
            overhead_ms: 200,
            tokens_in: 1200,
            tokens_out: 300,
            cache_read_tokens: 0,
            cost_usd: 0.0021,
            attempts: 1,
            errors: { rate_limited: 1 },
          },
          spans: [
            {
              id: 'sp1',
              trace_id: 'tr2',
              kind: 'llm_request',
              name: 'primary',
              start: '2024-01-02T10:01:00Z',
              end: '2024-01-02T10:01:02.500Z',
              status: 'ok',
            },
            {
              id: 'sp2',
              trace_id: 'tr2',
              kind: 'tool_execution',
              name: 'bash',
              start: '2024-01-02T10:01:02.500Z',
              end: '2024-01-02T10:01:03.500Z',
              status: 'error',
              error_class: 'timeout',
            },
          ],
        },
      ],
    }));
    const root = document.createElement('div');
    const view = hubView(fakeApi({ traces } as Partial<ApiClient>), { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#session-trace')).not.toBeNull());
    const panel = root.querySelector<HTMLElement>('#trace-panel')!;
    expect(panel.hidden).toBe(true);
    root.querySelector<HTMLButtonElement>('#session-trace')!.click();
    await vi.waitFor(() => expect(panel.hidden).toBe(false));
    await vi.waitFor(() => expect(panel.querySelectorAll('.trace-turn').length).toBe(1));
    expect(traces).toHaveBeenCalledWith('s1');
    const text = panel.textContent ?? '';
    expect(text).toContain('第 1 轮');
    expect(text).toContain('模型 2.5s');
    expect(text).toContain('rate_limited×1');
    expect(panel.querySelector('.trace-span-err')?.textContent).toContain('timeout');
    // Clicking again collapses without a second fetch...
    root.querySelector<HTMLButtonElement>('#session-trace')!.click();
    expect(panel.hidden).toBe(true);
    expect(traces).toHaveBeenCalledTimes(1);
    // ...but reopening re-reads the spans: a turn finished meanwhile must show.
    root.querySelector<HTMLButtonElement>('#session-trace')!.click();
    await vi.waitFor(() => expect(traces).toHaveBeenCalledTimes(2));
  });

  it('shows an empty trace panel when no spans were recorded', async () => {
    const root = document.createElement('div');
    const view = hubView(
      fakeApi({ traces: vi.fn(async () => ({ turns: [] })) } as Partial<ApiClient>),
      { openSessionId: 's1' },
    );
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#session-trace')).not.toBeNull());
    root.querySelector<HTMLButtonElement>('#session-trace')!.click();
    await vi.waitFor(() =>
      expect(root.querySelector('#trace-panel')?.textContent).toContain('暂无 trace 记录'),
    );
  });

  it('shows live context usage in the header instead of the cumulative total', async () => {
    const root = document.createElement('div');
    const view = hubView(
      fakeApi({
        getSession: vi.fn(async (id: string): Promise<Session> => ({
          ...session({ id, tokens_in: 742095, tokens_out: 11849 }),
          messages: [message()],
          // Context rides the transcript payload: no second history read.
          context: {
            context_limit: 128000,
            fixed_cost: 15000,
            history_tokens: 53000,
            history_bytes: 180000,
            used: 68000,
            soft_limit: 86000,
            hard_limit: 120000,
          },
        })),
      }),
      { openSessionId: 's1' },
    );
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#chat-meta .ctx')).not.toBeNull());
    const meta = root.querySelector<HTMLElement>('#chat-meta')!;
    // 主位是当前窗口占用（68k/128k = 53%），累计值降级为次要信息。
    expect(meta.textContent).toContain('上下文 68.0k / 128k · 53%');
    expect(meta.querySelector('.ctx')?.getAttribute('title')).toContain('压缩触发线 86000');
    expect(meta.querySelector('.dim')?.textContent).toContain('累计 754k tokens');
  });

  it('shows this session pending approval inline and decides it there', async () => {
    const api = fakeApi({
      listApprovals: vi.fn(async () => [
        {
          id: 'ap1',
          session_id: 's1',
          action: 'web_search',
          resource: 'chyan',
          created_at: '2024-01-01T00:00:00Z',
        },
        {
          id: 'ap2',
          session_id: 's2',
          action: 'bash',
          resource: 'ls',
          created_at: '2024-01-01T00:00:00Z',
        },
      ]),
      listTasks: vi.fn(async () => [task({ status: 'running' })]),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#session-approvals')).not.toBeNull());
    expect(root.querySelector('#session-approvals')?.textContent).toContain('web_search');
    // Another conversation's ticket never leaks into this transcript.
    expect(root.querySelector('#session-approvals [data-id="ap2"]')).toBeNull();
    // The ask replaces the generic "generating" placeholder.
    expect(root.querySelector('.msg-pending')).toBeNull();

    root
      .querySelector<HTMLButtonElement>('#session-approvals [data-decide="approve"]')!
      .click();
    await vi.waitFor(() =>
      expect(api.decideApproval).toHaveBeenCalledWith('ap1', { decision: 'approve' }),
    );
  });

  it('treats an already-resolved approval as done instead of an error', async () => {
    const api = fakeApi({
      listApprovals: vi.fn(async () => [
        {
          id: 'ap1',
          session_id: 's1',
          action: 'bash',
          resource: 'ls',
          created_at: '2024-01-01T00:00:00Z',
        },
      ]),
      listTasks: vi.fn(async () => [task({ status: 'running' })]),
      decideApproval: vi.fn(async () => {
        // Double click / another device already resolved it: the daemon says 409.
        throw new ApiError(409, 'approval ap1 not pending');
      }),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() =>
      expect(root.querySelector('#session-approvals [data-decide="approve"]')).not.toBeNull(),
    );
    const getSession = api.getSession as unknown as ReturnType<typeof vi.fn>;
    const before = getSession.mock.calls.length;
    root.querySelector<HTMLButtonElement>('#session-approvals [data-decide="approve"]')!.click();
    await vi.waitFor(() => expect(api.decideApproval).toHaveBeenCalled());
    // The transcript refreshes (card disappears) and no raw error is printed.
    await vi.waitFor(() => expect(getSession.mock.calls.length).toBeGreaterThan(before));
    expect(root.querySelector('#session-error .err-text')).toBeNull();
  });

  it('shows a failed run where the assistant reply belongs', async () => {
    const api = fakeApi({
      listTasks: vi.fn(async () => [
        task({
          status: 'failed',
          err: 'connection refused',
          created_at: '2024-01-01T00:00:00Z',
          updated_at: '2024-01-01T00:00:02.500Z',
        }),
      ]),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('.msg-failed')).not.toBeNull());
    expect(root.querySelector('.msg-failed')!.textContent).toContain('任务失败：connection refused');
    expect(root.querySelector('.msg-failed-meta')!.textContent).toContain('耗时 2.5s');
    expect(root.querySelector('#task-form-msg')!.textContent).toBe('');
  });

  it('retries the last user turn from the failed bubble', async () => {
    const api = fakeApi({
      listTasks: vi.fn(async () => [task({ status: 'failed', err: 'boom' })]),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('[data-retry]')).not.toBeNull());
    root.querySelector<HTMLButtonElement>('[data-retry]')!.click();
    await vi.waitFor(() =>
      expect(api.createTask).toHaveBeenCalledWith({
        prompt: 'hello',
        session_id: 's1',
        retry: true,
      }),
    );
  });

  it('stops a running turn from the transcript header', async () => {
    const api = fakeApi({
      listTasks: vi.fn(async () => [task({ status: 'running' })]),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#session-stop')).not.toBeNull());
    root.querySelector<HTMLButtonElement>('#session-stop')!.click();
    await vi.waitFor(() => expect(api.cancelTask).toHaveBeenCalledWith('t1'));
  });

  it('shows usage totals and the last turn duration', async () => {
    const api = fakeApi({
      getSession: vi.fn(async (id: string): Promise<Session> => ({
        ...session({ id, title: id, tokens_in: 1000, tokens_out: 234, cost_usd: 0.0042 }),
        messages: [message()],
      })),
      listTasks: vi.fn(async () => [
        task({
          status: 'done',
          created_at: '2024-01-01T00:00:00Z',
          updated_at: '2024-01-01T00:00:02.500Z',
        }),
      ]),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() =>
      expect(root.querySelector('#chat-meta')?.textContent).toContain('累计 1.2k tokens'),
    );
    const meta = root.querySelector('#chat-meta')!.textContent ?? '';
    expect(meta).toContain('$0.0042');
    expect(meta).toContain('上一轮 2.5s');
  });

  it('hides the usage meta row when nothing is reported', async () => {
    const api = fakeApi({
      listTasks: vi.fn(async () => [task({ status: 'running' })]),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#chat-meta')).not.toBeNull());
    expect(root.querySelector('#chat-meta')!.textContent).toBe('');
    expect(root.querySelector('#session-stop')).not.toBeNull();
  });

  it('offers no stop control once the run is terminal', async () => {
    const api = fakeApi({
      listTasks: vi.fn(async () => [task({ status: 'done' })]),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('.chat-head')).not.toBeNull());
    expect(root.querySelector('#session-stop')).toBeNull();
  });

  it('reports a failed cancel and keeps the stop control enabled', async () => {
    const api = fakeApi({
      listTasks: vi.fn(async () => [task({ status: 'running' })]),
      cancelTask: vi.fn(async () => {
        throw new Error('cancel boom');
      }),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#session-stop')).not.toBeNull());
    const stop = root.querySelector<HTMLButtonElement>('#session-stop')!;
    stop.click();
    await vi.waitFor(() => expect(root.querySelector('#session-error')?.textContent).toContain('cancel boom'));
    expect(stop.disabled).toBe(false);
  });

  it('does not retry when the transcript has no user turn', async () => {
    const api = fakeApi({
      listTasks: vi.fn(async () => [task({ status: 'failed', err: 'boom' })]),
      getSession: vi.fn(async (id: string): Promise<Session> => ({
        ...session({ id, title: id }),
        messages: [message({ role: 'assistant', text: 'only reply' })],
      })),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('[data-retry]')).not.toBeNull());
    root.querySelector<HTMLButtonElement>('[data-retry]')!.click();
    expect(api.createTask).not.toHaveBeenCalled();
  });

  it('wires the assistant feedback actions as a toggle pair', async () => {
    const api = fakeApi({
      getSession: vi.fn(async (id: string): Promise<Session> => ({
        ...session({ id, title: id }),
        messages: [message({ role: 'assistant', text: 'answer' })],
      })),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('[data-msg-action="like"]')).not.toBeNull());
    const like = root.querySelector<HTMLButtonElement>('[data-msg-action="like"]')!;
    const dislike = root.querySelector<HTMLButtonElement>('[data-msg-action="dislike"]')!;
    like.click();
    expect(like.classList.contains('is-on')).toBe(true);
    dislike.click();
    expect(dislike.classList.contains('is-on')).toBe(true);
    expect(like.classList.contains('is-on')).toBe(false);
  });

  it('appends streamed output to the live reply for the open session', async () => {
    const root = document.createElement('div');
    const view = hubView(fakeApi(), { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('.msg-list')).not.toBeNull());
    view.onEvent?.({ name: 'task.output', data: { session_id: 's1', text: 'Hello ' } });
    await vi.waitFor(() =>
      expect(root.querySelector('#live-reply')?.textContent).toContain('Hello'),
    );
    view.onEvent?.({ name: 'task.output', data: { session_id: 's1', text: '**world**' } });
    await vi.waitFor(() =>
      expect(root.querySelector('#live-reply strong')?.textContent).toBe('world'),
    );
    // Deltas from another conversation never leak into this transcript.
    view.onEvent?.({ name: 'task.output', data: { session_id: 's2', text: 'nope' } });
    expect(root.querySelector('#live-reply')?.textContent).not.toContain('nope');
  });

  it('ignores status-only output events and non-output events', async () => {
    const root = document.createElement('div');
    const view = hubView(fakeApi(), { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('.msg-list')).not.toBeNull());
    // updateTask publishes the full reply without a session id.
    view.onEvent?.({ name: 'task.output', data: { id: 't1', status: 'done', text: 'final' } });
    view.onEvent?.({ name: 'task.started', data: { id: 't1', session_id: 's1' } });
    expect(root.querySelector('#live-reply')).toBeNull();
  });

  it('drops the live bubble once the run is persisted and terminal', async () => {
    const api = fakeApi();
    const getSession = api.getSession as ReturnType<typeof vi.fn>;
    getSession.mockResolvedValue({
      ...session({ id: 's1', title: 's1' }),
      messages: [message()],
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('.msg-list')).not.toBeNull());
    view.onEvent?.({ name: 'task.output', data: { session_id: 's1', text: 'partial' } });
    await vi.waitFor(() => expect(root.querySelector('#live-reply')).not.toBeNull());

    getSession.mockResolvedValue({
      ...session({ id: 's1', title: 's1' }),
      messages: [message(), message({ id: 'a1', role: 'assistant', text: 'final answer' })],
    });
    (api.listTasks as ReturnType<typeof vi.fn>).mockResolvedValue([task({ status: 'done' })]);
    view.refresh?.(root);
    await vi.waitFor(() => expect(root.querySelector('#live-reply')).toBeNull());
    expect(root.querySelector('.msg-assistant .msg-text')?.textContent).toContain('final answer');
  });

  it('hydrates attached images and opens them in the viewer', async () => {
    const createObjectURL = vi.fn(() => 'blob:img');
    Object.defineProperty(URL, 'createObjectURL', { value: createObjectURL, configurable: true });
    const api = fakeApi({
      getSession: vi.fn(async (id: string): Promise<Session> => ({
        ...session({ id, title: id }),
        messages: [message({ images: ['/tmp/a.png'] })],
      })),
      downloadFile: vi.fn(async () => new Blob(['x'], { type: 'image/png' })),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('.msg-image')?.getAttribute('src')).toBe('blob:img'));
    expect(api.downloadFile).toHaveBeenCalledWith('/tmp/a.png', '', 'artifacts');
    root.querySelector<HTMLImageElement>('.msg-image')!.click();
    expect(root.querySelector<HTMLElement>('#image-viewer')!.hidden).toBe(false);
  });

  it('marks an image that cannot be downloaded as broken', async () => {
    const root = document.createElement('div');
    const api = fakeApi({
      getSession: vi.fn(async (id: string): Promise<Session> => ({
        ...session({ id, title: id }),
        messages: [message({ images: ['/tmp/missing.png'] })],
      })),
      downloadFile: vi.fn(async () => {
        throw new Error('404');
      }),
    });
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() =>
      expect(root.querySelector('.msg-image')!.classList.contains('msg-image-broken')).toBe(true),
    );
  });

  it('reuses cached image urls across refreshes and revokes them on cleanup', async () => {
    const createObjectURL = vi.fn(() => 'blob:img');
    const revokeObjectURL = vi.fn();
    Object.defineProperty(URL, 'createObjectURL', { value: createObjectURL, configurable: true });
    Object.defineProperty(URL, 'revokeObjectURL', { value: revokeObjectURL, configurable: true });
    const api = fakeApi({
      getSession: vi.fn(async (id: string): Promise<Session> => ({
        ...session({ id, title: id }),
        messages: [message({ images: ['/tmp/a.png'] })],
      })),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('.msg-image')?.getAttribute('src')).toBe('blob:img'));
    view.refresh?.(root);
    await vi.waitFor(() => expect(root.querySelector('.msg-image')?.getAttribute('src')).toBe('blob:img'));
    // A refresh must not re-download or invalidate the image.
    expect(createObjectURL).toHaveBeenCalledTimes(1);
    expect(revokeObjectURL).not.toHaveBeenCalled();
    view.cleanup?.();
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:img');
  });

  it('continues the session from the inline composer', async () => {
    const api = fakeApi();
    const onCreated = vi.fn();
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1', onCreated });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#task-prompt')).not.toBeNull());
    root.querySelector<HTMLTextAreaElement>('#task-prompt')!.value = 'again';
    root.querySelector<HTMLFormElement>('#task-form')!.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true }),
    );
    await vi.waitFor(() =>
      expect(api.createTask).toHaveBeenCalledWith({ prompt: 'again', session_id: 's1' }),
    );
    expect(onCreated).toHaveBeenCalledWith('s-new');
  });

  it('opens the execution timeline', async () => {
    const onOpenTask = vi.fn();
    const root = document.createElement('div');
    const view = hubView(fakeApi(), { openSessionId: 's1', onOpenTask });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#session-timeline')).not.toBeNull());
    root.querySelector<HTMLButtonElement>('#session-timeline')!.click();
    expect(onOpenTask).toHaveBeenCalledWith('t1');
  });

  it('renames the session through the in-page dialog', async () => {
    const api = fakeApi();
    const onSessionsChanged = vi.fn();
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1', onSessionsChanged });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#session-rename')).not.toBeNull());
    root.querySelector<HTMLButtonElement>('#session-rename')!.click();
    await vi.waitFor(() => expect(document.querySelector('.dialog-input')).not.toBeNull());
    const input = document.querySelector<HTMLInputElement>('.dialog-input')!;
    expect(input.value).toBe('s1');
    expect(input.maxLength).toBe(20);
    input.value = '新标题';
    document.querySelector<HTMLButtonElement>('[data-dialog-ok]')!.click();
    await vi.waitFor(() => expect(api.renameSession).toHaveBeenCalledWith('s1', '新标题'));
    expect(onSessionsChanged).toHaveBeenCalled();
  });

  it('does not rename when the dialog is cancelled', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#session-rename')).not.toBeNull());
    root.querySelector<HTMLButtonElement>('#session-rename')!.click();
    await vi.waitFor(() => expect(document.querySelector('.dialog-input')).not.toBeNull());
    document.querySelector<HTMLButtonElement>('[data-dialog-cancel]')!.click();
    expect(api.renameSession).not.toHaveBeenCalled();
  });

  it('deletes the session and returns to the list', async () => {
    const onOpenSession = vi.fn();
    const api = fakeApi();
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1', onOpenSession });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#session-delete')).not.toBeNull());
    root.querySelector<HTMLButtonElement>('#session-delete')!.click();
    await vi.waitFor(() =>
      expect(document.querySelector('[data-dialog-ok]')).not.toBeNull(),
    );
    document.querySelector<HTMLButtonElement>('[data-dialog-ok]')!.click();
    await vi.waitFor(() => expect(api.deleteSession).toHaveBeenCalledWith('s1'));
    expect(onOpenSession).toHaveBeenCalledWith(null);
  });

  it('keeps the session when the delete dialog is cancelled', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#session-delete')).not.toBeNull());
    root.querySelector<HTMLButtonElement>('#session-delete')!.click();
    await vi.waitFor(() => expect(document.querySelector('[data-dialog-cancel]')).not.toBeNull());
    document.querySelector<HTMLButtonElement>('[data-dialog-cancel]')!.click();
    expect(api.deleteSession).not.toHaveBeenCalled();
  });
});

describe('readDataURL', () => {
  it('resolves a base64 data URL', async () => {
    const url = await readDataURL(new File(['hello'], 'a.txt', { type: 'text/plain' }));
    expect(url).toBe('data:text/plain;base64,aGVsbG8=');
  });
});

describe('legacyCopy', () => {
  it('copies through execCommand for plain-http origins', () => {
    const exec = vi.fn(() => true);
    Object.defineProperty(document, 'execCommand', { value: exec, configurable: true });
    expect(legacyCopy('hello')).toBe(true);
    expect(exec).toHaveBeenCalledWith('copy');
    expect(document.querySelectorAll('textarea')).toHaveLength(0);
  });

  it('lets the copy action write the assistant reply', async () => {
    const exec = vi.fn(() => true);
    Object.defineProperty(document, 'execCommand', { value: exec, configurable: true });
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
    const api = fakeApi({
      getSession: vi.fn(async (id: string): Promise<Session> => ({
        ...session({ id, title: id }),
        messages: [message({ role: 'assistant', text: '答案文本' })],
      })),
    });
    const root = document.createElement('div');
    const view = hubView(api, { openSessionId: 's1' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('[data-msg-action="copy"]')).not.toBeNull());
    root.querySelector<HTMLButtonElement>('[data-msg-action="copy"]')!.click();
    expect(exec).toHaveBeenCalledWith('copy');
  });
});
