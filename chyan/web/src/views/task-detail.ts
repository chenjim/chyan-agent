// One task's execution timeline: persisted transcript, live output, tool
// progress, pending approvals, cancellation and final error (FR-12 W2).

import type { ApiClient } from '../api';
import { esc, fmtTime, taskStatusLabel } from '../format';
import { chip, emptyState, errorState, skeleton, type View } from '../render';
import type { ApprovalTicket, Message, Task } from '../types';

export interface TaskDetailOptions {
  onBack?: () => void;
  onOpenSession?: (sessionId: string) => void;
}

interface ProgressLine {
  kind: string;
  tool?: string;
  detail?: string;
  elapsed_ms?: number;
  err?: string;
}

const LONG_OUTPUT = 400;

export function taskDetailView(api: ApiClient, taskId: string, options: TaskDetailOptions = {}): View {
  let disposed = false;
  let task: Task | null = null;
  let messages: Message[] | null = null;
  let approvals: ApprovalTicket[] = [];
  let live = '';
  let rootEl: HTMLElement | null = null;
  let renderTimer: number | null = null;
  const progress: ProgressLine[] = [];

  // Streaming arrives per token; coalesce DOM updates instead of re-rendering
  // the whole timeline on every delta.
  const scheduleRender = () => {
    if (renderTimer !== null || !rootEl) return;
    renderTimer = window.setTimeout(() => {
      renderTimer = null;
      if (!disposed && rootEl) render(rootEl);
    }, 100);
  };

  const shell = () => `
    <section class="page task-detail">
      <div class="row">
        <button class="btn ghost" id="task-back">← 任务</button>
        <span class="section-title">执行时间线</span>
      </div>
      <div id="task-head"></div>
      <div id="task-timeline">${skeleton(3)}</div>
      <div id="task-detail-msg" class="form-msg"></div>
    </section>`;

  const loadTask = async () => {
    task = await api.getTask(taskId);
    return task;
  };

  const loadSession = async () => {
    if (!task?.session_id) return;
    try {
      const s = await api.getSession(task.session_id);
      messages = s.messages;
    } catch {
      messages = null; // transcript unavailable: live output still renders
    }
  };

  const loadApprovals = async () => {
    try {
      const all = await api.listApprovals();
      approvals = task?.session_id ? all.filter((a) => a.session_id === task?.session_id) : [];
    } catch {
      approvals = [];
    }
  };

  const reload = async (root: HTMLElement, withSession: boolean) => {
    try {
      await loadTask();
      if (withSession) await loadSession();
      await loadApprovals();
      if (!disposed) render(root);
    } catch (err) {
      if (disposed) return;
      const box = root.querySelector<HTMLElement>('#task-timeline');
      if (box) box.innerHTML = errorState(err);
    }
  };

  const render = (root: HTMLElement) => {
    const head = root.querySelector<HTMLElement>('#task-head');
    if (head) head.innerHTML = task ? renderHead(task) : '';
    const timeline = root.querySelector<HTMLElement>('#task-timeline');
    if (!timeline) return;
    timeline.innerHTML = renderTimeline(task, messages, live, progress, approvals);
    const cancel = root.querySelector<HTMLButtonElement>('[data-cancel-task]');
    cancel?.addEventListener('click', () => {
      cancel.disabled = true;
      void (async () => {
        try {
          await api.cancelTask(taskId);
          await reload(root, false);
        } catch (err) {
          cancel.disabled = false;
          const msg = root.querySelector<HTMLElement>('#task-detail-msg');
          if (msg) msg.textContent = `取消失败：${err instanceof Error ? err.message : err}`;
        }
      })();
    });
    root.querySelector('#task-open-session')?.addEventListener('click', () => {
      if (task?.session_id) options.onOpenSession?.(task.session_id);
    });
    timeline.querySelectorAll<HTMLElement>('[data-approval]').forEach((card) => {
      card.querySelectorAll<HTMLButtonElement>('[data-decision]').forEach((btn) => {
        btn.addEventListener('click', () => {
          const id = card.dataset.approval ?? '';
          const decision = btn.dataset.decision ?? '';
          btn.disabled = true;
          void (async () => {
            try {
              await api.decideApproval(id, { decision, reason: '' });
              await loadApprovals();
              if (!disposed) render(root);
            } catch (err) {
              btn.disabled = false;
              const msg = root.querySelector<HTMLElement>('#task-detail-msg');
              if (msg) msg.textContent = `审批失败：${err instanceof Error ? err.message : err}`;
            }
          })();
        });
      });
    });
  };

  return {
    html: shell(),
    mounted(root) {
      rootEl = root;
      root.querySelector('#task-back')?.addEventListener('click', () => options.onBack?.());
      void reload(root, true);
    },
    refresh(root) {
      // SSE refreshes must not wipe the live buffer mid-run: only reload the
      // task state and approvals until the task reaches a terminal state.
      const terminal = task && task.status !== 'pending' && task.status !== 'running';
      void reload(root, Boolean(terminal));
    },
    onEvent(ev) {
      const d = ev.data as Record<string, unknown> | null;
      if (!d || String(d.id ?? '') !== taskId) return;
      switch (ev.name) {
        case 'task.output':
          if (typeof d.text === 'string') live += d.text;
          if (typeof d.status === 'string' && task) task.status = d.status;
          break;
        case 'task.progress':
          progress.push({
            kind: String(d.kind ?? ''),
            tool: typeof d.tool === 'string' ? d.tool : undefined,
            detail: typeof d.detail === 'string' ? d.detail : undefined,
            elapsed_ms: typeof d.elapsed_ms === 'number' ? d.elapsed_ms : undefined,
            err: typeof d.err === 'string' && d.err ? d.err : undefined,
          });
          break;
        case 'task.done':
          if (typeof d.status === 'string' && task) task.status = d.status;
          if (typeof d.reply === 'string') live = d.reply;
          if (typeof d.err === 'string' && d.err) live = '';
          if (rootEl) void reload(rootEl, true);
          return;
      }
      scheduleRender();
    },
    cleanup() {
      disposed = true;
      rootEl = null;
      if (renderTimer !== null) window.clearTimeout(renderTimer);
      renderTimer = null;
    },
  };
}

function renderHead(t: Task): string {
  const running = t.status === 'pending' || t.status === 'running';
  return `
    <div class="card">
      <div class="row">
        ${chip(taskStatusLabel(t.status), `st-${t.status}`)}
        <span class="mono dim">${esc(t.id.slice(0, 8))}</span>
        <span class="dim time">${fmtTime(t.created_at)}</span>
        ${running ? `<button class="btn small" type="button" data-cancel-task="1">取消</button>` : ''}
        ${t.session_id ? `<button class="btn small ghost" type="button" id="task-open-session">打开会话</button>` : ''}
      </div>
    </div>`;
}

function renderTimeline(
  task: Task | null,
  messages: Message[] | null,
  live: string,
  progress: ProgressLine[],
  approvals: ApprovalTicket[],
): string {
  if (!task) return emptyState('任务不存在');
  const parts: string[] = [];
  const shown = messages ?? [];
  for (const m of shown) parts.push(renderTimelineMessage(m));
  if (live && task.status === 'running') {
    parts.push(`
      <div class="msg msg-assistant">
        <div class="row">${chip('助手', 'role-assistant')}<span class="dim">生成中…</span></div>
        <div class="msg-text">${renderText(live)}</div>
      </div>`);
  }
  if (progress.length) {
    parts.push(`
      <div class="card tool-progress">
        <div class="dim section-title">工具进度</div>
        ${progress.map(renderProgress).join('')}
      </div>`);
  }
  for (const a of approvals) parts.push(renderApproval(a));
  if (task.err) parts.push(`<div class="card err-text">${esc(task.err)}</div>`);
  if (task.reply && task.status !== 'running') {
    parts.push(`
      <div class="msg msg-assistant">
        <div class="row">${chip('助手', 'role-assistant')}</div>
        <div class="msg-text">${renderText(task.reply)}</div>
      </div>`);
  }
  if (!parts.length) return emptyState('等待任务输出…');
  return parts.join('');
}

function renderTimelineMessage(m: Message): string {
  if (m.role === 'tool') {
    return `
      <div class="msg msg-tool">
        <div class="row">${chip('工具', 'role-tool')}<span class="dim time">${fmtTime(m.created_at)}</span></div>
        ${foldOutput(m.text)}
      </div>`;
  }
  const role = m.role === 'user' ? '用户' : m.role === 'assistant' ? '助手' : m.role;
  const tools = (m.tool_calls ?? []).map((t) => chip(t, 'mono dim')).join('');
  return `
    <div class="msg msg-${esc(m.role)}">
      <div class="row">${chip(role, `role-${esc(m.role)}`)}<span class="dim time">${fmtTime(m.created_at)}</span></div>
      <div class="msg-text">${renderText(m.text)}</div>
      ${tools ? `<div class="row tool-row">${tools}</div>` : ''}
    </div>`;
}

function renderProgress(p: ProgressLine): string {
  const elapsed = p.elapsed_ms ? ` ${p.elapsed_ms}ms` : '';
  if (p.kind === 'tool_run') {
    return `<div class="dim">▸ ${esc(p.tool || 'tool')}${esc(elapsed)} ${esc(p.detail || '')}</div>`;
  }
  if (p.kind === 'tool_done') {
    return p.err
      ? `<div class="err-text">✕ ${esc(p.tool || 'tool')}${esc(elapsed)}：${esc(p.err)}</div>`
      : `<div class="dim">✓ ${esc(p.tool || 'tool')}${esc(elapsed)}</div>`;
  }
  return `<div class="dim">${esc(p.detail || p.kind)}</div>`;
}

function renderApproval(a: ApprovalTicket): string {
  return `
    <div class="card approval-card" data-approval="${esc(a.id)}">
      <div class="row">${chip('待审批', 'st-pending')}${esc(a.action)}</div>
      <div class="mono dim">${esc(a.resource)}</div>
      ${a.diff ? `<pre class="diff">${esc(a.diff)}</pre>` : ''}
      <div class="row">
        <button class="btn small primary" data-decision="approve">批准</button>
        <button class="btn small" data-decision="deny">拒绝</button>
      </div>
    </div>`;
}

// foldOutput collapses long tool output; artifacts stay reachable as links.
function foldOutput(text: string): string {
  const body = renderText(text);
  if (text.length <= LONG_OUTPUT) return `<div class="msg-text">${body}</div>`;
  return `
    <details class="tool-output">
      <summary>工具输出（${text.length} 字）</summary>
      <div class="msg-text">${body}</div>
    </details>`;
}

// renderText escapes content and turns [artifact: path] markers into links to
// the read-only file viewer (route #/files/<path>).
export function renderText(text: string): string {
  return text
    .split(/(\[artifact: [^\]]+\])/)
    .map((part) => {
      const m = /^\[artifact: (.+)\]$/.exec(part);
      if (!m) return esc(part);
      const path = m[1].trim();
      return `<a class="artifact-link" href="#/artifacts/${encodeURIComponent(path)}">artifact: ${esc(path)}</a>`;
    })
    .join('');
}
