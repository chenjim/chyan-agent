// @vitest-environment happy-dom
import { describe, expect, it, vi } from 'vitest';
import { ApiError, type ApiClient } from '../api';
import type { DreamReport, MemoryEntry, WorkspaceInfo } from '../types';
import { memoryView, renderMemory } from './memory';

function entry(over: Partial<MemoryEntry> = {}): MemoryEntry {
  return {
    id: 'm1',
    kind: 'fact',
    domain: 'global',
    name: '部署目录',
    content: 'prod deploy dir is /srv/app',
    status: 'active',
    path: '/mem/fact/m1.md',
    created_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

function report(over: Partial<DreamReport> = {}): DreamReport {
  return { Examined: 5, Merged: 1, Superseded: 1, Orphaned: 2, Notes: ['项目归属待确认'], ...over };
}

function fakeApi(over: Partial<ApiClient> = {}): ApiClient {
  return {
    listMemory: vi.fn(async () => [entry()]),
    supersedeMemory: vi.fn(async () => entry({ status: 'superseded' })),
    getDream: vi.fn(async () => Promise.reject(new ApiError(404, 'no dream report yet'))),
    runDream: vi.fn(async () => report()),
    getWorkspace: vi.fn(async (): Promise<WorkspaceInfo> => ({ path: '/repo', recent: ['/repo'] })),
    ...over,
  } as unknown as ApiClient;
}

async function mount(api: ApiClient) {
  const root = document.createElement('div');
  const view = memoryView(api);
  root.innerHTML = view.html;
  view.mounted?.(root);
  await vi.waitFor(() => expect(root.querySelector('.memory-card')).not.toBeNull());
  return { root, view };
}

describe('memoryView', () => {
  it('lists memories and marks one invalid', async () => {
    const api = fakeApi();
    const { root } = await mount(api);
    expect(root.querySelector('.memory-card')?.textContent).toContain('部署目录');

    root.querySelector<HTMLButtonElement>('[data-memory-supersede]')!.click();
    await vi.waitFor(() => expect(document.querySelector('[data-dialog-ok]')).not.toBeNull());
    document.querySelector<HTMLButtonElement>('[data-dialog-ok]')!.click();
    await vi.waitFor(() => expect(api.supersedeMemory).toHaveBeenCalledWith('m1', ''));
    await vi.waitFor(() => expect(root.querySelector('#memory-body')?.textContent).toContain('部署目录'));
  });

  it('searches and filters by domain', async () => {
    const api = fakeApi({
      listMemory: vi.fn(async () => [entry(), entry({ id: 'm2', name: '项目事实', domain: 'project' })]),
    });
    const { root } = await mount(api);
    const search = root.querySelector<HTMLInputElement>('#memory-search')!;
    search.value = '部署';
    search.dispatchEvent(new Event('input', { bubbles: true }));
    await vi.waitFor(() => expect(api.listMemory).toHaveBeenLastCalledWith(
      expect.objectContaining({ q: '部署', domain: '', kind: '' }),
    ));

    const domain = root.querySelector<HTMLSelectElement>('#memory-domain')!;
    domain.value = 'project';
    domain.dispatchEvent(new Event('change', { bubbles: true }));
    await vi.waitFor(() => expect(api.listMemory).toHaveBeenLastCalledWith(
      expect.objectContaining({ domain: 'project' }),
    ));
  });

  it('shows the latest dream report and runs a new pass', async () => {
    const api = fakeApi({ getDream: vi.fn(async () => report()) });
    const { root } = await mount(api);
    await vi.waitFor(() => expect(root.querySelector('#dream-report')?.textContent).toContain('检查 5'));

    api.runDream = vi.fn(async () => report({ Examined: 9, Merged: 2 }));
    root.querySelector<HTMLButtonElement>('#dream-run')!.click();
    await vi.waitFor(() => expect(root.querySelector('#dream-report')?.textContent).toContain('检查 9'));
  });

  it('reports a missing dream report and API failures', async () => {
    const api = fakeApi();
    const { root } = await mount(api);
    await vi.waitFor(() => expect(root.querySelector('#dream-report')?.textContent).toContain('尚未运行'));

    api.runDream = vi.fn(async () => Promise.reject(new Error('<bad>')));
    root.querySelector<HTMLButtonElement>('#dream-run')!.click();
    await vi.waitFor(() => expect(root.querySelector('#dream-report')?.textContent).toContain('<bad>'));
  });

  it('shows empty state and escaped list errors', async () => {
    const api = fakeApi({ listMemory: vi.fn(async () => []) });
    const root = document.createElement('div');
    const view = memoryView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('#memory-body')?.textContent).toContain('没有匹配的记忆'));

    const bad = fakeApi({ listMemory: vi.fn(async () => Promise.reject(new Error('<denied>'))) });
    const root2 = document.createElement('div');
    const view2 = memoryView(bad);
    root2.innerHTML = view2.html;
    view2.mounted?.(root2);
    await vi.waitFor(() => expect(root2.querySelector('#memory-body')?.textContent).toContain('加载失败：<denied>'));
  });
});

describe('renderMemory', () => {
  it('escapes content and folds long bodies', () => {
    const short = renderMemory(entry({ content: '<danger>' }));
    expect(short).toContain('&lt;danger&gt;');
    expect(short).not.toContain('<details>');
    const long = renderMemory(entry({ content: 'x'.repeat(500) }));
    expect(long).toContain('<details>');
    expect(renderMemory(entry({ keywords: ['k'], triggers: ['t'] }))).toContain('触发:t');
  });
});

describe('memoryView edge paths', () => {
  it('preselects a project, lists workspace options and shows dream notes', async () => {
    const api = fakeApi({ getDream: vi.fn(async () => report({ Notes: ['<note>'] })) });
    const root = document.createElement('div');
    const view = memoryView(api, { project: '/repo' });
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('.memory-card')).not.toBeNull());
    expect(root.querySelector<HTMLInputElement>('#memory-project')?.value).toBe('/repo');
    expect(root.querySelector('#memory-projects')?.innerHTML).toContain('/repo');
    expect(root.querySelector('#dream-report')?.textContent).toContain('<note>');
  });

  it('reports a supersede failure in the list region', async () => {
    const api = fakeApi({ supersedeMemory: vi.fn(async () => Promise.reject(new Error('<denied>'))) });
    const { root } = await mount(api);
    root.querySelector<HTMLButtonElement>('[data-memory-supersede]')!.click();
    await vi.waitFor(() => expect(document.querySelector('[data-dialog-ok]')).not.toBeNull());
    document.querySelector<HTMLButtonElement>('[data-dialog-ok]')!.click();
    await vi.waitFor(() => expect(root.querySelector('#memory-body')?.textContent).toContain('加载失败：<denied>'));
  });

  it('renders a report without notes and cleans up a pending search timer', async () => {
    const api = fakeApi({ getDream: vi.fn(async () => report({ Notes: undefined })) });
    const { root, view } = await mount(api);
    await vi.waitFor(() => expect(root.querySelector('#dream-report')?.textContent).toContain('检查 5'));
    expect(root.querySelector('.dream-notes')).toBeNull();
    const search = root.querySelector<HTMLInputElement>('#memory-search')!;
    search.dispatchEvent(new Event('input', { bubbles: true }));
    view.cleanup?.();
    await new Promise((r) => setTimeout(r, 400));
  });
});
