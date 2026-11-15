// @vitest-environment happy-dom
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api';
import type { CronJob, WorkspaceInfo } from '../types';
import { cronView, renderCronJob } from './cron';

function job(over: Partial<CronJob> = {}): CronJob {
  return {
    id: 'j1',
    name: '日报',
    schedule: '@every 1h',
    prompt: '写日报',
    enabled: true,
    ...over,
  };
}

function fakeApi(over: Partial<ApiClient> = {}): ApiClient {
  return {
    listCron: vi.fn(async () => [job()]),
    createCron: vi.fn(async (req) => job({ ...req, id: 'new' })),
    updateCron: vi.fn(async (id, req) => job({ ...req, id })),
    deleteCron: vi.fn(async () => undefined),
    runCron: vi.fn(async () => ({ started: true })),
    getWorkspace: vi.fn(async (): Promise<WorkspaceInfo> => ({ path: '/repo', recent: ['/repo'] })),
    ...over,
  } as unknown as ApiClient;
}

describe('cronView', () => {
  it('lists jobs and runs/toggles/edits them', async () => {
    const api = fakeApi();
    const onOpenSession = vi.fn();
    const root = document.createElement('div');
    const view = cronView(api, { onOpenSession });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('.cron-card')).not.toBeNull());

    root.querySelector<HTMLButtonElement>('[data-cron-run]')!.click();
    await vi.waitFor(() => expect(api.runCron).toHaveBeenCalledWith('j1'));

    root.querySelector<HTMLButtonElement>('[data-cron-toggle]')!.click();
    await vi.waitFor(() => expect(api.updateCron).toHaveBeenCalledWith('j1', expect.objectContaining({ enabled: false })));

    root.querySelector<HTMLButtonElement>('[data-cron-edit]')!.click();
    expect(root.querySelector<HTMLInputElement>('#cron-name')?.value).toBe('日报');
    expect(root.querySelector<HTMLButtonElement>('#cron-save')?.textContent).toBe('保存修改');
    root.querySelector<HTMLButtonElement>('#cron-cancel')!.click();
    expect(root.querySelector<HTMLButtonElement>('#cron-save')?.textContent).toBe('保存');

    root.querySelector<HTMLButtonElement>('[data-cron-delete]')!.click();
    await vi.waitFor(() => expect(document.querySelector('[data-dialog-ok]')).not.toBeNull());
    document.querySelector<HTMLButtonElement>('[data-dialog-ok]')!.click();
    await vi.waitFor(() => expect(api.deleteCron).toHaveBeenCalledWith('j1'));
    expect(onOpenSession).not.toHaveBeenCalled();
  });

  it('creates a job from the form and rejects an empty one', async () => {
    const api = fakeApi({ listCron: vi.fn(async () => []) });
    const root = document.createElement('div');
    const view = cronView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#cron-body')?.textContent).toContain('还没有定时任务'));

    root.querySelector<HTMLFormElement>('#cron-form')!.dispatchEvent(new Event('submit', { cancelable: true }));
    expect(root.querySelector('#cron-form-error')?.textContent).toContain('不能为空');

    root.querySelector<HTMLInputElement>('#cron-name')!.value = '新任务';
    root.querySelector<HTMLInputElement>('#cron-schedule')!.value = '0 9 * * *';
    root.querySelector<HTMLTextAreaElement>('#cron-prompt')!.value = '早报';
    root.querySelector<HTMLFormElement>('#cron-form')!.dispatchEvent(new Event('submit', { cancelable: true }));
    await vi.waitFor(() =>
      expect(api.createCron).toHaveBeenCalledWith(expect.objectContaining({ name: '新任务', schedule: '0 9 * * *' })),
    );
    expect(root.querySelector<HTMLInputElement>('#cron-name')?.value).toBe('');
  });

  it('opens the session of the last run and refreshes on cron SSE', async () => {
    const onOpenSession = vi.fn();
    const api = fakeApi({ listCron: vi.fn(async () => [job({ last_session_id: 's1', last_status: 'ok' })]) });
    const root = document.createElement('div');
    const view = cronView(api, { onOpenSession });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('[data-cron-session="s1"]')).not.toBeNull());
    root.querySelector<HTMLButtonElement>('[data-cron-session="s1"]')!.click();
    expect(onOpenSession).toHaveBeenCalledWith('s1');

    api.listCron = vi.fn(async () => [job({ name: '刷新后' })]);
    view.onEvent?.({ name: 'cron.run', data: { id: 'j1' } });
    await vi.waitFor(() => expect(root.querySelector('.cron-card')?.textContent).toContain('刷新后'));
  });

  it('shows load errors and workspace datalist options', async () => {
    const api = fakeApi({ listCron: vi.fn(async () => Promise.reject(new Error('<down>'))) });
    const root = document.createElement('div');
    const view = cronView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#cron-body')?.textContent).toContain('加载失败：<down>'));
    expect(root.querySelector('#cron-workdirs')?.innerHTML).toContain('/repo');
  });
});

describe('renderCronJob', () => {
  it('marks success, failure and disabled states', () => {
    const ok = renderCronJob(job({ last_status: 'ok', last_run_at: '2024-01-01T00:00:00Z' }));
    expect(ok).toContain('上次成功');
    expect(ok).toContain('已启用');
    const err = renderCronJob(job({ last_status: 'error', last_error: '<boom>' }));
    expect(err).toContain('上次失败');
    expect(err).toContain('&lt;boom&gt;');
    expect(renderCronJob(job({ enabled: false }))).toContain('已停用');
    expect(renderCronJob(job({ schedule: '' }))).toContain('事件触发');
  });
});
