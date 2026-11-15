// Read-only project file tree: browse directories, view file content. No editing.

import type { ApiClient } from '../api';
import { downloadBlob } from '../download';
import { esc, fmtSize } from '../format';
import { emptyState, errorState, skeleton, type View } from '../render';
import type { FileNode } from '../types';

export function filesView(api: ApiClient, initialPath = '', openAsFile = false, source = ''): View {
  let disposed = false;
  const html = `
    <section class="page">
      <div class="row">
        <button class="btn ghost" id="file-up">↑ 上级</button>
        <span class="mono dim path-label" id="file-path"></span>
      </div>
      <div id="file-body">${skeleton(5)}</div>
    </section>`;

  // A deep link to a file (#/files/<path>) opens its preview; the up button
  // still works because cwd starts at its directory.
  const initialDir = openAsFile && initialPath.includes('/')
    ? initialPath.slice(0, initialPath.lastIndexOf('/'))
    : openAsFile
      ? ''
      : initialPath;
  let cwd = initialDir;
  // What the body currently shows; drives in-place SSE refreshes.
  let shown: { path: string; isDir: boolean } | null = null;

  const renderDir = async (root: HTMLElement, path: string) => {
    const box = root.querySelector<HTMLElement>('#file-body');
    const label = root.querySelector<HTMLElement>('#file-path');
    if (!box) return;
    shown = { path, isDir: true };
    if (label) label.textContent = path || '/';
    try {
      const nodes = await api.listFiles(path, '', source);
      if (disposed) return;
      box.innerHTML = nodes.length
        ? `<div class="file-list">${nodes.map((n) => renderNode(n)).join('')}</div>`
        : emptyState('空目录');
    } catch (err) {
      if (!disposed) box.innerHTML = errorState(err);
    }
  };

  // showSkeleton=false is used by SSE refreshes so a preview does not flash.
  const renderFile = async (root: HTMLElement, path: string, showSkeleton = true) => {
    const box = root.querySelector<HTMLElement>('#file-body');
    const label = root.querySelector<HTMLElement>('#file-path');
    if (!box) return;
    shown = { path, isDir: false };
    if (label) label.textContent = path;
    if (showSkeleton) box.innerHTML = skeleton(2);
    try {
      const file = await api.readFile(path, '', source);
      if (disposed) return;
      box.innerHTML = `
        <div class="card">
          <div class="row">
            <button class="btn small" id="file-copy">复制路径</button>
            <button class="btn small" id="file-download">下载</button>
          </div>
          ${file.truncated ? '<div class="dim">（内容已截断）</div>' : ''}
          <pre class="file-content">${esc(file.content)}</pre>
        </div>`;
      box.querySelector('#file-copy')?.addEventListener('click', () => {
        void navigator.clipboard?.writeText(path);
      });
      box.querySelector('#file-download')?.addEventListener('click', () => {
        void (async () => {
          try {
            const blob = await api.downloadFile(path, '', source);
            downloadBlob(blob, path.split('/').pop() || 'file');
          } catch (err) {
            const msg = root.querySelector<HTMLElement>('#file-body');
            if (msg) msg.innerHTML = errorState(err);
          }
        })();
      });
    } catch (err) {
      if (!disposed) box.innerHTML = errorState(err);
    }
  };

  return {
    html,
    mounted(root) {
      root.addEventListener('click', (e) => {
        const row = (e.target as HTMLElement).closest<HTMLElement>('[data-file-path]');
        if (!row) return;
        const path = row.dataset.filePath ?? '';
        if (row.dataset.isDir === '1') {
          cwd = path;
          void renderDir(root, path);
        } else {
          void renderFile(root, path);
        }
      });
      root.querySelector('#file-up')?.addEventListener('click', () => {
        const parent = cwd.includes('/') ? cwd.slice(0, cwd.lastIndexOf('/')) : '';
        cwd = parent;
        void renderDir(root, parent);
      });
      if (openAsFile && initialPath) void renderFile(root, initialPath);
      else void renderDir(root, cwd);
    },
    refresh(root) {
      if (!shown) return;
      if (shown.isDir) void renderDir(root, shown.path);
      else void renderFile(root, shown.path, false);
    },
    cleanup() {
      disposed = true;
    },
  };
}

const FOLDER_ICON =
  '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3.2 7A1.8 1.8 0 015 5.2h4.2l2 2H19a1.8 1.8 0 011.8 1.8v8A1.8 1.8 0 0119 18.8H5A1.8 1.8 0 013.2 17V7z"/></svg>';
const FILE_ICON =
  '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 3.2h7l5 5v12.6H6z"/><path d="M13 3.2v5h5"/></svg>';

export function renderNode(n: FileNode): string {
  const icon = n.is_dir ? FOLDER_ICON : FILE_ICON;
  const size = n.is_dir ? '' : `<span class="dim">${fmtSize(n.size ?? 0)}</span>`;
  return `
    <div class="file-row" data-file-path="${esc(n.path)}" data-is-dir="${n.is_dir ? '1' : '0'}">
      <span class="file-icon">${icon}</span>
      <span class="file-name">${esc(n.name)}</span>
      ${size}
    </div>`;
}
