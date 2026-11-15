// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const sseMock = vi.hoisted(() => ({ opens: [] as any[], closes: 0 }));
const apiMock = vi.hoisted(() => ({
  status: vi.fn(),
  listTasks: vi.fn(),
  listSessions: vi.fn(),
  listApprovals: vi.fn(),
  listFiles: vi.fn(),
  readFile: vi.fn(),
  search: vi.fn(),
  deleteSession: vi.fn(),
}));

vi.mock('./sse', () => ({
  openSse: (opts: any) => {
    sseMock.opens.push(opts);
    return { close: () => void sseMock.closes++ };
  },
}));

vi.mock('./api', () => ({
  ApiError: class ApiError extends Error {
    status: number;
    constructor(status: number) {
      super('api error');
      this.status = status;
    }
  },
  ApiClient: class {
    constructor(_opts: { token: string }) {
      void _opts;
    }
    async status() {
      return apiMock.status();
    }
    async listTasks() {
      return apiMock.listTasks();
    }
    async listSessions() {
      return apiMock.listSessions();
    }
    async listApprovals() {
      return apiMock.listApprovals();
    }
    async listFiles() {
      return apiMock.listFiles();
    }
    async readFile(path: string) {
      return apiMock.readFile(path);
    }
    async search(q: string) {
      return apiMock.search(q);
    }
    async createTask() {
      return { id: 't' };
    }
    async cancelTask() {}
    async deleteSession(id: string) {
      return apiMock.deleteSession(id);
    }
    async decideApproval() {}
    async getSession(id: string) {
      return { id, created_at: '', updated_at: '', messages: [] };
    }
    // 页头读当前窗口占用；桩里不给预算，按"没有数据就不显示"渲染。
    async sessionContext() {
      return undefined;
    }
  },
}));

import { ApiError } from './api';
import { App } from './main';

const STATUS = {
  version: '0.1.0', sessions: 3, pending_approvals: 0, running_tasks: 2, review_mode: 'ask',
};

function newRoot(): HTMLElement {
  const root = document.createElement('div');
  root.id = 'app';
  document.body.appendChild(root);
  return root;
}

async function login(root: HTMLElement): Promise<void> {
  root.querySelector<HTMLInputElement>('#login-token')!.value = 'tok';
  root.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(root.querySelector('.topbar')).not.toBeNull());
}

beforeEach(() => {
  localStorage.clear();
  // Tab switches write the route hash; reset it so each test boots on 任务.
  window.history.replaceState(null, '', '/');
  sseMock.opens.length = 0;
  sseMock.closes = 0;
  apiMock.status.mockResolvedValue(STATUS);
  apiMock.listTasks.mockResolvedValue([]);
  apiMock.listSessions.mockResolvedValue([]);
  apiMock.listApprovals.mockResolvedValue([]);
  apiMock.listFiles.mockResolvedValue([]);
  apiMock.readFile.mockResolvedValue({ path: 'p', content: '' });
  apiMock.search.mockResolvedValue([]);
  apiMock.deleteSession.mockResolvedValue(undefined);
  vi.clearAllMocks();
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.useRealTimers();
  vi.unstubAllGlobals();
  Object.defineProperty(document, 'hidden', { value: false, configurable: true });
});

describe('App login gate', () => {
  it('shows the login form without a stored token', () => {
    const root = newRoot();
    new App(root);
    expect(root.querySelector('#login-form')).not.toBeNull();
    expect(sseMock.opens).toHaveLength(0);
  });

  it('stores the token and boots the shell after login', async () => {
    const root = newRoot();
    new App(root);
    await login(root);
    expect(localStorage.getItem('chyan_token')).toBe('tok');
    expect(root.querySelector('#content')).not.toBeNull();
    // One conversation entry plus the three settings tools: approvals no
    // longer owns a navigation slot (asks are decided inside the transcript).
    expect(root.querySelectorAll('[data-tab]')).toHaveLength(4);
    expect(root.querySelector('[data-tab="approvals"]')).toBeNull();
    expect(root.querySelector('.side-approval')).toBeNull();
    expect(sseMock.opens).toHaveLength(1);
    // The sidebar status line is gone; the version now lives in 设置 > 关于.
    expect(root.querySelector('#status-text')).toBeNull();
    await vi.waitFor(() => {
      expect(root.querySelector('#about-version')?.textContent).toBe('v0.1.0');
    });
  });

  it('ignores an empty token submit', () => {
    const root = newRoot();
    new App(root);
    root.querySelector('#login-form')!.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true }),
    );
    expect(root.querySelector('#login-form')).not.toBeNull();
    expect(sseMock.opens).toHaveLength(0);
  });
});

describe('App navigation', () => {
  it('switches tabs in place and marks the active one', async () => {
    const root = newRoot();
    const app = new App(root);
    await login(root);
    const btn = root.querySelector<HTMLElement>('[data-tab="files"]')!;
    app.handleClick({ target: btn } as unknown as Event);
    expect(btn.classList.contains('active')).toBe(true);
    await vi.waitFor(() => expect(apiMock.listFiles).toHaveBeenCalled());
    // Content stays the same element (shell is not remounted).
    expect(root.querySelector('#content')).not.toBeNull();
  });

  it('merges tasks and sessions into one conversation entry', async () => {
    const root = newRoot();
    new App(root);
    await login(root);
    // The single hub tab loads both data sets for one unified list.
    await vi.waitFor(() => {
      expect(apiMock.listSessions).toHaveBeenCalled();
      expect(apiMock.listTasks).toHaveBeenCalled();
    });
  });

  it('keeps the approvals page reachable by URL without a nav entry', async () => {
    // The view still exists for direct links, but it owns no navigation slot.
    window.history.replaceState(null, '', '#/approvals');
    const root = newRoot();
    new App(root);
    await login(root);
    await vi.waitFor(() => expect(apiMock.listApprovals).toHaveBeenCalled());
  });

  it('lists sessions in the sidebar and opens one as the transcript', async () => {
    apiMock.listSessions.mockResolvedValue([
      {
        id: 's1',
        title: '侧栏会话',
        created_at: '2024-01-01T00:00:00Z',
        updated_at: '2024-01-01T00:00:00Z',
      },
    ]);
    const root = newRoot();
    new App(root);
    await login(root);
    await vi.waitFor(() => expect(root.querySelector('[data-side-session="s1"]')).not.toBeNull());
    root.querySelector<HTMLElement>('[data-side-session="s1"]')!.click();
    await vi.waitFor(() => expect(root.querySelector('.detail-composer')).not.toBeNull());
    // The sidebar row mirrors the open session.
    expect(root.querySelector('[data-side-session="s1"]')?.classList.contains('active')).toBe(true);
  });

  it('multi-selects sidebar sessions and deletes the checked ones', async () => {
    apiMock.listSessions.mockResolvedValue([
      { id: 's1', title: '会话一', created_at: '2024-01-01T00:00:00Z', updated_at: '2024-01-01T00:00:00Z' },
      { id: 's2', title: '会话二', created_at: '2024-01-02T00:00:00Z', updated_at: '2024-01-02T00:00:00Z' },
    ]);
    const root = newRoot();
    new App(root);
    await login(root);
    await vi.waitFor(() => expect(root.querySelectorAll('[data-side-session]')).toHaveLength(2));

    root.querySelector<HTMLButtonElement>('[data-side-select]')!.click();
    expect(root.querySelector('.side-select-bar')).not.toBeNull();
    root.querySelector<HTMLElement>('[data-side-session="s1"]')!.click();
    root.querySelector<HTMLElement>('[data-side-session="s2"]')!.click();
    expect(root.querySelector('.side-select-count')?.textContent).toContain('2');
    // Selecting never opens a transcript.
    expect(root.querySelector('.detail-composer')).toBeNull();

    root.querySelector<HTMLButtonElement>('[data-side-delete]')!.click();
    await vi.waitFor(() => expect(document.querySelector('[data-dialog-ok]')).not.toBeNull());
    document.querySelector<HTMLButtonElement>('[data-dialog-ok]')!.click();
    await vi.waitFor(() => expect(apiMock.deleteSession).toHaveBeenCalledWith('s1'));
    expect(apiMock.deleteSession).toHaveBeenCalledWith('s2');
    await vi.waitFor(() => expect(root.querySelector('.side-select-bar')).toBeNull());
  });

  it('keeps selections when the delete dialog is cancelled', async () => {
    apiMock.listSessions.mockResolvedValue([
      { id: 's1', title: '会话一', created_at: '2024-01-01T00:00:00Z', updated_at: '2024-01-01T00:00:00Z' },
    ]);
    const root = newRoot();
    new App(root);
    await login(root);
    await vi.waitFor(() => expect(root.querySelector('[data-side-session="s1"]')).not.toBeNull());

    root.querySelector<HTMLButtonElement>('[data-side-select]')!.click();
    root.querySelector<HTMLElement>('[data-side-session="s1"]')!.click();
    root.querySelector<HTMLButtonElement>('[data-side-delete]')!.click();
    await vi.waitFor(() => expect(document.querySelector('[data-dialog-cancel]')).not.toBeNull());
    document.querySelector<HTMLButtonElement>('[data-dialog-cancel]')!.click();
    expect(apiMock.deleteSession).not.toHaveBeenCalled();
    expect(root.querySelector('.side-select-bar')).not.toBeNull();
  });

  it('selects every sidebar session from the select-all action', async () => {
    apiMock.listSessions.mockResolvedValue([
      { id: 's1', title: '会话一', created_at: '2024-01-01T00:00:00Z', updated_at: '2024-01-01T00:00:00Z' },
      { id: 's2', title: '会话二', created_at: '2024-01-02T00:00:00Z', updated_at: '2024-01-02T00:00:00Z' },
    ]);
    const root = newRoot();
    new App(root);
    await login(root);
    await vi.waitFor(() => expect(root.querySelectorAll('[data-side-session]')).toHaveLength(2));

    root.querySelector<HTMLButtonElement>('[data-side-select]')!.click();
    root.querySelector<HTMLButtonElement>('[data-side-select-all]')!.click();
    expect(root.querySelector('.side-select-count')?.textContent).toContain('2');
    expect(root.querySelector('[data-side-session="s1"]')?.getAttribute('aria-pressed')).toBe('true');
    root.querySelector<HTMLButtonElement>('[data-side-cancel]')!.click();
    expect(root.querySelector('.side-select-bar')).toBeNull();
    expect(root.querySelector('.side-check')).toBeNull();
  });

  it('drops checks for sessions removed by a sidebar refresh', async () => {
    apiMock.listSessions.mockResolvedValue([
      { id: 's1', title: '会话一', created_at: '2024-01-01T00:00:00Z', updated_at: '2024-01-01T00:00:00Z' },
      { id: 's2', title: '会话二', created_at: '2024-01-02T00:00:00Z', updated_at: '2024-01-02T00:00:00Z' },
    ]);
    const root = newRoot();
    new App(root);
    await login(root);
    await vi.waitFor(() => expect(root.querySelectorAll('[data-side-session]')).toHaveLength(2));
    root.querySelector<HTMLButtonElement>('[data-side-select]')!.click();
    root.querySelector<HTMLElement>('[data-side-session="s1"]')!.click();
    expect(root.querySelector('.side-select-count')?.textContent).toContain('1');

    // Another client deletes the checked session; the SSE refresh must prune
    // the stale id so the count and delete action stay consistent.
    apiMock.listSessions.mockResolvedValue([
      { id: 's2', title: '会话二', created_at: '2024-01-02T00:00:00Z', updated_at: '2024-01-02T00:00:00Z' },
    ]);
    sseMock.opens[0].onEvent({ name: 'session.updated', data: {} });
    await vi.waitFor(() => expect(root.querySelector('[data-side-session="s1"]')).toBeNull());
    expect(root.querySelector('.side-select-count')?.textContent).toContain('0');
    expect(root.querySelector<HTMLButtonElement>('[data-side-delete]')?.disabled).toBe(true);
  });

  it('opens the content search dialog from the header entry', async () => {
    const root = newRoot();
    new App(root);
    await login(root);
    root.querySelector<HTMLButtonElement>('#search-open')!.click();
    expect(root.querySelector<HTMLElement>('#search-overlay')!.hidden).toBe(false);
    root.querySelector<HTMLButtonElement>('#search-close')!.click();
    expect(root.querySelector<HTMLElement>('#search-overlay')!.hidden).toBe(true);
  });

  it('cycles the sidebar through expanded, rail and hidden states', async () => {
    const root = newRoot();
    new App(root);
    await login(root);
    const shell = root.querySelector<HTMLElement>('.shell')!;
    expect(shell.classList.contains('sidebar-expanded')).toBe(true);
    root.querySelector<HTMLButtonElement>('#sidebar-toggle')!.click();
    expect(shell.classList.contains('sidebar-collapsed')).toBe(true);
    expect(localStorage.getItem('chyan_sidebar')).toBe('collapsed');
    root.querySelector<HTMLButtonElement>('#rail-toggle')!.click();
    expect(shell.classList.contains('sidebar-hidden')).toBe(true);
    root.querySelector<HTMLButtonElement>('#shell-menu')!.click();
    expect(shell.classList.contains('sidebar-expanded')).toBe(true);
    expect(localStorage.getItem('chyan_sidebar')).toBe('expanded');
  });

  it('ignores clicks outside the tab bar', async () => {
    const root = newRoot();
    const app = new App(root);
    await login(root);
    app.handleClick({ target: root.querySelector('.brand-block') } as unknown as Event);
    expect(root.querySelector('[data-tab="tasks"]')?.classList.contains('active')).toBe(true);
  });

  it('opens settings for secondary tools and closes it after navigating', async () => {
    const root = newRoot();
    const app = new App(root);
    await login(root);
    const menu = root.querySelector<HTMLElement>('#settings-menu')!;
    const toggle = root.querySelector<HTMLButtonElement>('#settings-toggle')!;
    expect(menu.hidden).toBe(true);
    toggle.click();
    expect(menu.hidden).toBe(false);
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(root.querySelector('[data-tab="files"]')).not.toBeNull();

    app.handleClick({ target: root.querySelector('[data-tab="files"]') } as unknown as Event);
    expect(menu.hidden).toBe(true);
    await vi.waitFor(() => expect(apiMock.listFiles).toHaveBeenCalled());
    expect(root.querySelector('#settings-toggle')?.classList.contains('active')).toBe(true);
  });

  it('closes the settings sheet with Escape and restores focus', async () => {
    const root = newRoot();
    new App(root);
    await login(root);
    const toggle = root.querySelector<HTMLButtonElement>('#settings-toggle')!;
    const menu = root.querySelector<HTMLElement>('#settings-menu')!;
    toggle.click();
    expect(menu.hidden).toBe(false);
    const firstItem = menu.querySelector<HTMLElement>('.tab')!;
    expect(document.activeElement).toBe(firstItem);
    firstItem.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    expect(menu.hidden).toBe(true);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(document.activeElement).toBe(toggle);
  });

  it('switches theme from the settings sheet', async () => {
    const root = newRoot();
    new App(root);
    await login(root);
    root.querySelector<HTMLButtonElement>('#settings-toggle')!.click();
    const dark = root.querySelector<HTMLElement>('[data-theme-option="dark"]')!;
    dark.click();
    expect(document.documentElement.dataset.theme).toBe('dark');
    expect(localStorage.getItem('chyan_theme')).toBe('dark');
    expect(dark.getAttribute('aria-pressed')).toBe('true');
    root.querySelector<HTMLElement>('[data-theme-option="system"]')!.click();
    expect(localStorage.getItem('chyan_theme')).toBe('system');
    expect(dark.getAttribute('aria-pressed')).toBe('false');
  });

  it('logs out from the settings sheet', async () => {
    const root = newRoot();
    new App(root);
    await login(root);
    root.querySelector<HTMLButtonElement>('#settings-toggle')!.click();
    root.querySelector<HTMLButtonElement>('#logout')!.click();
    expect(root.querySelector('#login-form')).not.toBeNull();
    expect(localStorage.getItem('chyan_token')).toBeNull();
  });
});

describe('App SSE handling', () => {
  it('updates the badge and title without rebuilding the content', async () => {
    const root = newRoot();
    new App(root);
    await login(root);
    const content = root.querySelector('#content');
    const sse = sseMock.opens[sseMock.opens.length - 1];
    sse.onEvent({ name: 'approval.requested', data: { id: 'a1' } });
    sse.onEvent({ name: 'approval.requested', data: { id: 'a2' } });
    // No nav badge survives, but the approval count still drives the title.
    expect(root.querySelector('[data-approval-badge]')).toBeNull();
    expect(document.title).toBe('(2) chyan 呈言智');
    expect(root.querySelector('#content')).toBe(content);
    await new Promise((r) => setTimeout(r, 400));
    // The hub and the sidebar each refresh once, coalesced per surface.
    expect(apiMock.listTasks.mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it('coalesces a burst into one view refresh and skips task.output', async () => {
    const root = newRoot();
    new App(root);
    await login(root);
    const sse = sseMock.opens[sseMock.opens.length - 1];
    // Wait for the boot fetches (hub + sidebar) before measuring the burst.
    await vi.waitFor(() => {
      expect(root.querySelector('#side-sessions')?.textContent).toContain('还没有会话');
    });
    const before = apiMock.listTasks.mock.calls.length;
    sse.onEvent({ name: 'task.started', data: { id: 't1' } });
    sse.onEvent({ name: 'task.done', data: { id: 't1' } });
    sse.onEvent({ name: 'approval.requested', data: { id: 'a1' } });
    await new Promise((r) => setTimeout(r, 400));
    // One coalesced refresh for the hub and one for the sidebar.
    expect(apiMock.listTasks.mock.calls.length).toBeLessThanOrEqual(before + 2);
    expect(apiMock.listTasks.mock.calls.length).toBeGreaterThan(before);

    const afterBurst = apiMock.listTasks.mock.calls.length;
    sse.onEvent({ name: 'task.output', data: { id: 't1', text: 'x' } });
    await new Promise((r) => setTimeout(r, 400));
    expect(apiMock.listTasks.mock.calls.length).toBe(afterBurst);
  });

  it('reconnects with backoff after the stream closes', async () => {
    vi.useFakeTimers();
    const root = newRoot();
    new App(root);
    root.querySelector<HTMLInputElement>('#login-token')!.value = 'tok';
    root.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.advanceTimersByTimeAsync(0);
    expect(sseMock.opens).toHaveLength(1);
    sseMock.opens[0].onClose();
    // Offline still reschedules the stream, not only after the retry.
    await vi.advanceTimersByTimeAsync(1000);
    expect(sseMock.opens).toHaveLength(2);
    expect(sseMock.closes).toBeGreaterThanOrEqual(1);
  });

  it('shows a task result toast and keeps handling events after a stream drop', async () => {
    const root = newRoot();
    new App(root);
    await login(root);
    const sse = sseMock.opens[sseMock.opens.length - 1];
    sse.onEvent({ name: 'task.done', data: { id: 't1', status: 'done' } });
    const toast = root.querySelector<HTMLElement>('#toast')!;
    expect(toast.hidden).toBe(false);
    expect(toast.textContent).toBe('任务已完成');

    sse.onClose();
    sse.onEvent({ name: 'task.done', data: { id: 't2', status: 'failed' } });
    expect(toast.textContent).toBe('任务失败');
  });

  it('requests notification permission only from the explicit button', async () => {
    const request = vi.fn(async () => 'granted' as NotificationPermission);
    const notifications: { title: string; body?: string }[] = [];
    class FakeNotification {
      static permission: NotificationPermission = 'default';
      static requestPermission = request;
      constructor(title: string, opts?: { body?: string }) {
        notifications.push({ title, body: opts?.body });
      }
    }
    vi.stubGlobal('Notification', FakeNotification);
    const root = newRoot();
    new App(root);
    await login(root);
    const btn = root.querySelector<HTMLButtonElement>('#notify-toggle')!;
    expect(btn.hidden).toBe(false);
    expect(request).not.toHaveBeenCalled(); // no automatic prompt
    btn.click();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(1));
    await vi.waitFor(() => expect(btn.hidden).toBe(true));

    // With permission granted a hidden page gets an OS notification.
    FakeNotification.permission = 'granted';
    Object.defineProperty(document, 'hidden', { value: true, configurable: true });
    sseMock.opens[sseMock.opens.length - 1].onEvent({ name: 'task.done', data: { id: 't3', status: 'done' } });
    expect(notifications).toHaveLength(1);
    expect(notifications[0].body).toBe('任务已完成');
    vi.stubGlobal('Notification', undefined);
  });

  it('logs out and shows the login form on a 401 status', async () => {
    apiMock.status.mockRejectedValueOnce(new ApiError(401, 'unauthorized'));
    const root = newRoot();
    localStorage.setItem('chyan_token', 'stale');
    const app = new App(root);
    void app;
    await vi.waitFor(() => expect(root.querySelector('#login-form')).not.toBeNull());
    expect(localStorage.getItem('chyan_token')).toBeNull();
    expect(sseMock.closes).toBeGreaterThanOrEqual(1);
  });
});
