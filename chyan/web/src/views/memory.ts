// Long-term memory view: search/browse memories by domain and kind, mark a
// wrong memory invalid, and view/run the dream maintenance pass (FR-12 W1).

import type { ApiClient } from '../api';
import { confirmDialog } from '../dialog';
import { esc, fmtTime } from '../format';
import { chip, emptyState, errorState, skeleton, type View } from '../render';
import type { DreamReport, MemoryEntry } from '../types';

export interface MemoryViewOptions {
  /** Project path preselected from the active workspace. */
  project?: string;
}

export function memoryView(api: ApiClient, options: MemoryViewOptions = {}): View {
  let disposed = false;
  let rootEl: HTMLElement | null = null;
  let timer: number | null = null;
  const html = `
    <section class="page">
      <h2 class="section-title">记忆</h2>
      <div class="card memory-filters">
        <input id="memory-search" class="input" placeholder="搜索名称/关键词/正文" />
        <div class="row">
          <select id="memory-domain" class="input">
            <option value="all">全部域</option>
            <option value="global">全局</option>
            <option value="project">项目</option>
          </select>
          <select id="memory-kind" class="input">
            <option value="all">全部类型</option>
            <option value="fact">事实</option>
            <option value="kb">知识</option>
            <option value="model">模型</option>
            <option value="directive">指令</option>
          </select>
        </div>
        <input id="memory-project" class="input mono" placeholder="项目路径（可选）" list="memory-projects" />
        <datalist id="memory-projects"></datalist>
      </div>
      <div class="card memory-dream" id="memory-dream">
        <div class="row">
          <span class="section-title">梦境整理</span>
          <button class="btn small" id="dream-run">立即整理</button>
        </div>
        <div id="dream-report" class="dim">加载中…</div>
      </div>
      <div id="memory-body">${skeleton(4)}</div>
    </section>`;

  const body = () => rootEl?.querySelector<HTMLElement>('#memory-body') ?? null;
  const reportBox = () => rootEl?.querySelector<HTMLElement>('#dream-report') ?? null;

  const params = () => {
    const project = rootEl?.querySelector<HTMLInputElement>('#memory-project')?.value.trim() ?? '';
    const domain = rootEl?.querySelector<HTMLSelectElement>('#memory-domain')?.value ?? 'all';
    const kind = rootEl?.querySelector<HTMLSelectElement>('#memory-kind')?.value ?? 'all';
    return {
      q: rootEl?.querySelector<HTMLInputElement>('#memory-search')?.value.trim() ?? '',
      domain: domain === 'all' ? '' : domain,
      kind: kind === 'all' ? '' : kind,
      project: project || options.project || '',
    };
  };

  const load = async () => {
    if (!rootEl) return;
    const box = body();
    try {
      const entries = await api.listMemory(params());
      if (disposed) return;
      if (box) {
        box.innerHTML = entries.length
          ? `<div class="memory-list">${entries.map(renderMemory).join('')}</div>`
          : emptyState('没有匹配的记忆');
      }
    } catch (err) {
      if (!disposed && box) box.innerHTML = errorState(err);
    }
  };

  const scheduleLoad = () => {
    if (timer !== null) return;
    timer = window.setTimeout(() => {
      timer = null;
      void load();
    }, 300);
  };

  const renderDream = (rep: DreamReport | null, err?: unknown) => {
    const box = reportBox();
    if (!box) return;
    if (err) {
      const msg = err instanceof Error ? err.message : String(err);
      box.innerHTML = `<span class="dim">${esc(msg)}</span>`;
      return;
    }
    if (!rep) {
      box.innerHTML = '<span class="dim">尚未运行梦境整理</span>';
      return;
    }
    const notes = (rep.Notes ?? []).map((n) => `<li>${esc(n)}</li>`).join('');
    box.innerHTML = `
      <div class="row">
        <span>检查 ${rep.Examined}</span>
        <span>合并 ${rep.Merged}</span>
        <span>失效 ${rep.Superseded}</span>
        <span>孤儿 ${rep.Orphaned}</span>
      </div>
      ${notes ? `<ul class="dream-notes">${notes}</ul>` : ''}`;
  };

  const loadDream = async () => {
    try {
      renderDream(await api.getDream());
    } catch {
      // 404 means no pass has run yet; the view says so.
      renderDream(null);
    }
  };

  return {
    html,
    mounted(root) {
      rootEl = root;
      const search = root.querySelector<HTMLInputElement>('#memory-search');
      search?.addEventListener('input', scheduleLoad);
      root.querySelector('#memory-domain')?.addEventListener('change', () => void load());
      root.querySelector('#memory-kind')?.addEventListener('change', () => void load());

      root.addEventListener('click', (e) => {
        const el = (e.target as HTMLElement).closest<HTMLElement>('[data-memory-id]');
        if (!el) return;
        if (el.dataset.memorySupersede === undefined) return;
        const id = el.dataset.memoryId ?? '';
        void (async () => {
          const ok = await confirmDialog({
            title: '标记记忆失效',
            body: '文件会保留为历史，召回不再使用。',
            confirmText: '标记失效',
            danger: true,
          });
          if (!ok) return;
          try {
            await api.supersedeMemory(id, params().project);
            await load();
          } catch (err) {
            const box = body();
            if (box) box.innerHTML = errorState(err);
          }
        })();
      });

      root.querySelector('#dream-run')?.addEventListener('click', () => {
        void (async () => {
          renderDream(null);
          const box = reportBox();
          if (box) box.innerHTML = '<span class="dim">整理中…</span>';
          try {
            renderDream(await api.runDream());
            await load();
          } catch (err) {
            renderDream(null, err);
          }
        })();
      });

      const project = root.querySelector<HTMLInputElement>('#memory-project');
      if (options.project && project) project.value = options.project;
      void api.getWorkspace().then((w) => {
        if (disposed) return;
        const dl = root.querySelector<HTMLElement>('#memory-projects');
        if (dl) dl.innerHTML = w.recent.map((p) => `<option value="${esc(p)}"></option>`).join('');
      }).catch(() => undefined);

      void load();
      void loadDream();
    },
    refresh() {
      void load();
    },
    cleanup() {
      disposed = true;
      rootEl = null;
      if (timer !== null) window.clearTimeout(timer);
      timer = null;
    },
  };
}

export function renderMemory(m: MemoryEntry): string {
  const long = m.content.length > 400;
  const content = long
    ? `<details><summary class="dim">展开正文（${m.content.length} 字）</summary><pre class="memory-content">${esc(m.content)}</pre></details>`
    : `<pre class="memory-content">${esc(m.content)}</pre>`;
  const keywords = (m.keywords ?? []).map((k) => chip(k, 'dim')).join('');
  const triggers = (m.triggers ?? []).map((t) => chip(`触发:${t}`, 'dim')).join('');
  return `
    <div class="card memory-card">
      <div class="row">
        <span class="memory-name">${esc(m.name)}</span>
        ${chip(m.kind, 'mono')}${chip(m.domain)}
      </div>
      ${content}
      ${keywords || triggers ? `<div class="row">${keywords}${triggers}</div>` : ''}
      <div class="row">
        <span class="dim time">${fmtTime(m.created_at)}</span>
        <button class="btn small ghost" data-memory-id="${esc(m.id)}" data-memory-supersede="1">标记失效</button>
      </div>
    </div>`;
}
