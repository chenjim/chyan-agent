// @vitest-environment happy-dom
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api';
import type { FileContent, FileNode } from '../types';
import { filesView, renderNode } from './files';

function node(over: Partial<FileNode> = {}): FileNode {
  return { name: 'a.txt', path: 'a.txt', is_dir: false, size: 10, ...over };
}

describe('filesView actions', () => {
  it('opens a deep-linked file and downloads it', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    const view = filesView(api, 'README.md', true);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(root.querySelector('.file-content')?.textContent).toContain('# hi'));
    expect(root.querySelector('#file-copy')).not.toBeNull();
    root.querySelector<HTMLButtonElement>('#file-download')!.click();
    await vi.waitFor(() => expect(api.downloadFile).toHaveBeenCalledWith('README.md', '', ''));
  });

  it('passes the artifact source to the API', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    const view = filesView(api, '/abs/out.txt', true, 'artifacts');
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => expect(api.readFile).toHaveBeenCalledWith('/abs/out.txt', '', 'artifacts'));
  });
});

describe('renderNode', () => {
  it('renders a directory row without size', () => {
    const html = renderNode(node({ name: 'src', path: 'src', is_dir: true }));
    expect(html).toContain('data-is-dir="1"');
    expect(html).toContain('file-icon');
    expect(html).toContain('<svg');
    expect(html).not.toContain(' B</span>');
  });

  it('renders a file row with a human size and escaped path', () => {
    const html = renderNode(node({ name: 'x<y>.go', path: 'x<y>.go', size: 2048 }));
    expect(html).toContain('data-is-dir="0"');
    expect(html).toContain('2.0 KB');
    expect(html).toContain('x&lt;y&gt;.go');
  });

  it('defaults a missing size to 0 B', () => {
    expect(renderNode({ name: 'p', path: 'p', is_dir: false })).toContain('0 B');
  });
});

function fakeApi(over: Partial<ApiClient> = {}): ApiClient {
  return {
    listFiles: vi.fn(async () => [
      node({ name: 'src', path: 'src', is_dir: true }),
      node({ name: 'README.md', path: 'README.md', size: 100 }),
    ]),
    readFile: vi.fn(async (): Promise<FileContent> => ({ path: 'README.md', content: '# hi' })),
    downloadFile: vi.fn(async () => new Blob(['x'])),
    ...over,
  } as unknown as ApiClient;
}

describe('filesView', () => {
  it('lists the root on mount and browses into a directory', async () => {
    const api = fakeApi({
      listFiles: vi
        .fn()
        .mockResolvedValueOnce([node({ name: 'src', path: 'src', is_dir: true })])
        .mockResolvedValueOnce([node({ name: 'main.go', path: 'src/main.go' })]),
    });
    const root = document.createElement('div');
    const view = filesView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => {
      expect(root.querySelector('[data-file-path="src"]')).not.toBeNull();
    });
    root.querySelector<HTMLElement>('[data-file-path="src"]')!.click();
    await vi.waitFor(() => {
      expect(api.listFiles).toHaveBeenLastCalledWith('src', '', '');
      expect(root.querySelector('#file-path')?.textContent).toBe('src');
    });
  });

  it('opens a file preview and marks truncated content', async () => {
    const api = fakeApi({
      readFile: vi.fn(async () => ({ path: 'README.md', content: '# hi', truncated: true })),
    });
    const root = document.createElement('div');
    const view = filesView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => {
      expect(root.querySelector('[data-file-path="README.md"]')).not.toBeNull();
    });
    root.querySelector<HTMLElement>('[data-file-path="README.md"]')!.click();
    await vi.waitFor(() => {
      expect(root.querySelector('.file-content')?.textContent).toBe('# hi');
      expect(root.querySelector('#file-body')?.textContent).toContain('内容已截断');
    });
  });

  it('navigates up one level', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    const view = filesView(api, 'src/lib');
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => {
      expect(api.listFiles).toHaveBeenCalledWith('src/lib', '', '');
    });
    root.querySelector<HTMLButtonElement>('#file-up')!.click();
    await vi.waitFor(() => {
      expect(api.listFiles).toHaveBeenLastCalledWith('src', '', '');
    });
  });

  it('stays at the root when going up from it', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    const view = filesView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => {
      expect(api.listFiles).toHaveBeenCalledWith('', '', '');
    });
    root.querySelector<HTMLButtonElement>('#file-up')!.click();
    await vi.waitFor(() => {
      expect(api.listFiles).toHaveBeenLastCalledWith('', '', '');
    });
  });

  it('refresh re-renders the current directory and the open file', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    const view = filesView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => {
      expect(root.querySelector('[data-file-path="README.md"]')).not.toBeNull();
    });
    view.refresh?.(root);
    await vi.waitFor(() => {
      expect(api.listFiles).toHaveBeenCalledTimes(2);
    });
    root.querySelector<HTMLElement>('[data-file-path="README.md"]')!.click();
    await vi.waitFor(() => {
      expect(root.querySelector('.file-content')).not.toBeNull();
    });
    view.refresh?.(root);
    await vi.waitFor(() => {
      expect(api.readFile).toHaveBeenCalledTimes(2);
    });
  });

  it('shows errors as an escaped empty state', async () => {
    const api = fakeApi({ listFiles: vi.fn(async () => Promise.reject(new Error('<denied>'))) });
    const root = document.createElement('div');
    const view = filesView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => {
      expect(root.querySelector('#file-body')?.textContent).toContain('加载失败：<denied>');
    });
  });

  it('ignores a click without a path and stops after cleanup', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    const view = filesView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => {
      expect(root.querySelector('.file-row')).not.toBeNull();
    });
    root.querySelector('#file-body')!.appendChild(document.createElement('div')).click();
    view.cleanup?.();
    view.refresh?.(root);
    await new Promise((r) => setTimeout(r, 0));
  });
});
