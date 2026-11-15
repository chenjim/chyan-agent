// Unified conversation hub: one entry for both "tasks" and "sessions".
// The composer starts a run, the list shows every conversation (with its
// latest task state), and a row opens the transcript plus its timeline.

import { ApiError, type ApiClient } from '../api';
import { brandMarkSvg } from '../brand';
import { confirmDialog, promptDialog } from '../dialog';
import { clearDraft, loadDraft, saveDraft } from '../draft';
import { downloadBlob } from '../download';
import { esc, fmtCost, fmtDuration, fmtTime, fmtTokens, taskStatusLabel } from '../format';
import { chip, emptyState, errorState, skeleton, type View } from '../render';
import type {
  ApprovalTicket,
  ContextUsage,
  CreateTaskRequest,
  Message,
  SessionSummary,
  Task,
  TraceTurn,
} from '../types';
import { renderTicket } from './approvals';
import { renderMarkdown, renderMessage, renderSessionTitle } from './messages';

const HUB_DRAFT = 'task';

export interface HubOptions {
  /** Session route to open, or null for the list. */
  openSessionId?: string | null;
  /** Session preselected in the composer after "continue". */
  continueSessionId?: string | null;
  /** Desktop=false: the sidebar owns the list, so the page is composer-only. */
  showList?: boolean;
  onOpenSession?: (id: string | null) => void;
  onSessionsChanged?: () => void;
  onContinue?: (id: string) => void;
  onClearContinue?: () => void;
  onOpenTask?: (id: string) => void;
  /** A run was accepted; the shell should open its session transcript. */
  onCreated?: (sessionId: string) => void;
}

export function hubView(api: ApiClient, options: HubOptions = {}): View {
  let disposed = false;
  let rootEl: HTMLElement | null = null;
  let online = true;
  let currentId: string | null = options.openSessionId ?? null;
  let continueId: string | null = options.continueSessionId ?? null;
  let sessions: SessionSummary[] = [];
  let tasks: Task[] = [];
  // Pending asks, filtered into the open transcript by session id.
  let approvals: ApprovalTicket[] = [];
  let query = '';
  let status = '';
  // Images pasted or attached before sending (data URLs; cleared on success).
  let pendingImages: { name: string; dataUrl: string }[] = [];
  // Live token stream for the open transcript (task.output deltas).
  let liveSessionId: string | null = null;
  let liveText = '';
  // Object URLs for transcript image thumbnails (revoked when leaving the view).
  let imageUrls: string[] = [];
  const imageCache = new Map<string, string>();
  const imagePending = new Map<string, Promise<string | null>>();
  let imageEpoch = 0;
  // Coalesce streamed deltas into ~12fps Markdown renders.
  let liveTimer: number | null = null;
  // Last loaded transcript, used by "retry" to resend the last user turn.
  let currentMessages: Message[] = [];

  // The viewer lives outside .page: that element keeps a transform from its
  // rise animation, which would trap a fixed overlay inside it.
  const html = `
    <section class="page">
      <div id="hub-body">${skeleton(4)}</div>
    </section>
    <div class="image-viewer" id="image-viewer" hidden>
      <button class="image-viewer-close" id="image-viewer-close" type="button" aria-label="关闭预览">×</button>
      <img id="image-viewer-img" alt="图片预览" />
    </div>`;

  const latestTasks = (): Map<string, Task> => {
    const out = new Map<string, Task>();
    for (const t of tasks) {
      const prev = out.get(t.session_id);
      if (!prev || t.created_at > prev.created_at) out.set(t.session_id, t);
    }
    return out;
  };

  /** Wall-clock span of one task (queue wait + turn). */
  const turnDuration = (t: Task): number => {
    const start = Date.parse(t.created_at);
    const end = Date.parse(t.updated_at);
    return Number.isFinite(start) && Number.isFinite(end) ? Math.max(0, end - start) : 0;
  };

  /**
   * Live window usage — the number that decides whether the next turn still
   * fits. Cumulative tokens only grow and never say that, so they demote to a
   * tooltip.
   */
  const contextMarkup = (ctx?: ContextUsage): string => {
    if (!ctx || !(ctx.context_limit > 0)) return '';
    const used = ctx.used ?? (ctx.fixed_cost ?? 0) + (ctx.history_tokens ?? 0);
    const pct = Math.min(100, Math.round((used / ctx.context_limit) * 100));
    const level = pct >= 90 ? ' ctx-critical' : pct >= 70 ? ' ctx-warn' : '';
    const detail = `上下文 ${used} / ${ctx.context_limit} tokens（系统提示 ${ctx.fixed_cost} + 历史 ${ctx.history_tokens}，压缩触发线 ${ctx.soft_limit}）`;
    return `<span class="ctx${level}" title="${esc(detail)}">上下文 ${fmtTokens(used)} / ${fmtTokens(ctx.context_limit)} · ${pct}%</span>`;
  };

  /** Session usage plus the last turn's duration, for the transcript header. */
  const metaMarkup = (session: SessionSummary, latest?: Task, ctx?: ContextUsage): string => {
    const parts: string[] = [];
    const ctxHtml = contextMarkup(ctx);
    if (ctxHtml) parts.push(ctxHtml);
    const usage: string[] = [];
    const tokens = (session.tokens_in ?? 0) + (session.tokens_out ?? 0);
    if (tokens > 0) usage.push(`累计 ${fmtTokens(tokens)} tokens`);
    if ((session.cost_usd ?? 0) > 0) usage.push(fmtCost(session.cost_usd ?? 0));
    if (usage.length) {
      parts.push(`<span class="dim" title="累计用量（每轮重发历史，仅供成本参考）">${esc(usage.join(' · '))}</span>`);
    }
    if (latest && latest.status !== 'running' && latest.status !== 'pending') {
      parts.push(`<span class="dim">${esc(`上一轮 ${fmtDuration(turnDuration(latest))}`)}</span>`);
    }
    return parts.join(' · ');
  };

  /** A failed run reads like the assistant reply it never produced. */
  const failureMarkup = (latest?: Task): string => {
    if (!latest || latest.status !== 'failed') return '';
    const detail = (latest.err ?? '').trim() || '未返回错误详情';
    return `
      <div class="msg msg-assistant msg-failed">
        <div class="msg-text">任务失败：${esc(detail)}</div>
        <div class="dim msg-failed-meta">耗时 ${fmtDuration(turnDuration(latest))}</div>
        <div class="msg-failed-actions">
          <button class="btn small ghost" type="button" data-retry>重试</button>
        </div>
      </div>`;
  };

  /** Pending approvals for this conversation, shown where the run is blocked. */
  const approvalMarkup = (sessionId: string): string => {
    const mine = approvals.filter((a) => a.session_id === sessionId);
    if (!mine.length) return '';
    return `
      <div class="msg msg-assistant msg-approval" id="session-approvals">
        <div class="msg-text dim">需要你确认以下操作后才能继续</div>
        <div class="approval-stack">${mine.map((t) => renderTicket(t)).join('')}</div>
      </div>`;
  };

  /** Live assistant text streamed from task.output (row not persisted yet). */
  const liveMarkup = (): string => {
    if (!liveText) return '';
    return `
      <div class="msg msg-assistant msg-live" id="live-reply">
        <div class="msg-text md">${renderMarkdown(liveText)}</div>
      </div>`;
  };

  const pendingRunMarkup = (latest?: Task, blocked = false): string => {
    if (!latest || liveText || blocked) return '';
    if (latest.status === 'pending') {
      return `<div class="msg msg-assistant msg-pending"><div class="msg-text dim">排队中…</div></div>`;
    }
    if (latest.status === 'running') {
      return `<div class="msg msg-assistant msg-pending"><div class="msg-text dim">正在生成…</div></div>`;
    }
    return '';
  };

  const transcriptMarkup = (messages: Message[], latest?: Task, approvalBlock = ''): string => {
    const body = messages.map((m) => renderMessage(m)).join('');
    const failure = failureMarkup(latest);
    const live = liveMarkup() + pendingRunMarkup(latest, approvalBlock !== '');
    const content = body + failure + live + approvalBlock;
    return content || emptyState('无消息');
  };

  /** Paint the accumulated live text (called at most ~12fps). */
  const renderLive = (root: HTMLElement): void => {
    const list = root.querySelector<HTMLElement>('.msg-list');
    if (!list || !liveText) return;
    let node = list.querySelector<HTMLElement>('#live-reply');
    if (!node) {
      list.querySelector('.empty-state')?.remove();
      list.querySelector('.msg-pending')?.remove();
      list.insertAdjacentHTML('beforeend', liveMarkup());
      node = list.querySelector<HTMLElement>('#live-reply');
    } else {
      const textEl = node.querySelector<HTMLElement>('.msg-text');
      if (textEl) textEl.innerHTML = renderMarkdown(liveText);
    }
    // Follow the stream only when the reader is already near the bottom.
    if (list.scrollHeight - list.scrollTop - list.clientHeight < 120) {
      list.scrollTop = list.scrollHeight;
    }
  };

  /** Coalesce token deltas so a long reply does not re-render per token. */
  const scheduleLive = (root: HTMLElement): void => {
    if (liveTimer !== null) return;
    liveTimer = window.setTimeout(() => {
      liveTimer = null;
      renderLive(root);
    }, 90);
  };

  const releaseImages = (): void => {
    imageEpoch++;
    for (const url of imageUrls) URL.revokeObjectURL?.(url);
    imageUrls = [];
    imageCache.clear();
    imagePending.clear();
  };

  /** Fetch one transcript image, caching the blob URL by artifact path. */
  const loadImage = (path: string): Promise<string | null> => {
    const cached = imageCache.get(path);
    if (cached) return Promise.resolve(cached);
    const pending = imagePending.get(path);
    if (pending) return pending;
    const epoch = imageEpoch;
    const p = api.downloadFile(path, '', 'artifacts')
      .then((blob) => {
        const url = URL.createObjectURL(blob);
        if (epoch !== imageEpoch) {
          URL.revokeObjectURL?.(url);
          return null;
        }
        imageCache.set(path, url);
        imageUrls.push(url);
        return url;
      })
      .catch(() => null)
      .finally(() => {
        if (imagePending.get(path) === p) imagePending.delete(path);
      });
    imagePending.set(path, p);
    return p;
  };

  /** Reuse cached blob URLs so SSE refreshes do not re-download images. */
  const hydrateImages = (root: HTMLElement): void => {
    if (typeof URL.createObjectURL !== 'function') return;
    root.querySelectorAll<HTMLImageElement>('[data-msg-image]').forEach((img) => {
      const path = img.dataset.msgImage;
      if (!path) return;
      const cached = imageCache.get(path);
      if (cached) {
        img.src = cached;
        return;
      }
      void loadImage(path).then((url) => {
        if (disposed) return;
        if (!url) {
          img.classList.add('msg-image-broken');
          return;
        }
        img.src = url;
      });
    });
  };

  const continueTitle = (): string => {
    if (!continueId) return '';
    const s = sessions.find((x) => x.id === continueId);
    return s ? renderSessionTitle(s) : continueId.slice(0, 8);
  };

  const clipIcon =
    '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M8.5 12.5l5.2-5.2a3 3 0 014.2 4.2L10.5 19a5 5 0 01-7-7l7.3-7.3"/></svg>';
  const sendIcon = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 19V5"/><path d="M6 11l6-6 6 6"/></svg>';
  const backIcon = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M15 6l-6 6 6 6"/></svg>';
  const clockIcon =
    '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="7.5"/><path d="M12 8v4.5l3 2"/></svg>';
  const pencilIcon =
    '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 20l4.5-1 10-10-3.5-3.5-10 10z"/><path d="M13.5 6.5L17 10"/></svg>';
  const downloadIcon =
    '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 4v10"/><path d="M8 11l4 4 4-4"/><path d="M5 19h14"/></svg>';
  const trashIcon =
    '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4.5 7h15"/><path d="M9 7V5h6v2"/><path d="M6.5 7l.8 12h9.4l.8-12"/></svg>';
  const stopIcon =
    '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="7.5" y="7.5" width="9" height="9" rx="1.5"/></svg>';
  // Trace viewer toggle (FR-13/U4): the pulse reads as "per-turn attribution".
  const traceIcon =
    '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 13h4l2.5-6 3 12 2.5-6h6"/></svg>';
  const stopBtnMarkup = `<button class="icon-btn" id="session-stop" type="button"
            aria-label="停止生成" title="停止生成">${stopIcon}</button>`;

  const pendingMarkup = (): string =>
    pendingImages
      .map(
        (img, i) => `
        <div class="composer-image">
          <img src="${esc(img.dataUrl)}" alt="${esc(img.name)}" data-preview-index="${i}" />
          <button class="composer-image-remove" type="button" data-remove-image="${i}"
                  aria-label="移除图片">×</button>
        </div>`,
      )
      .join('');

  const composer = (showChip = true, compact = false): string => `
    <form id="task-form" class="composer">
      ${
        continueId && showChip
          ? `<div class="composer-continue" id="composer-continue">
               <span class="dim">继续会话：</span><strong>${esc(continueTitle())}</strong>
               <button class="btn small ghost" id="continue-clear" type="button">新会话</button>
             </div>`
          : ''
      }
      <div id="composer-images" class="composer-images" ${pendingImages.length ? '' : 'hidden'}>
        ${pendingMarkup()}
      </div>
      <textarea id="task-prompt" class="input" rows="${compact ? 1 : 2}" placeholder="给 chyan 发消息…"></textarea>
      <div class="composer-row">
        ${
          compact
            ? ''
            : `<input id="task-workspace" class="input small" list="workspace-list" placeholder="工作区" />
        <datalist id="workspace-list"></datalist>
        <input id="task-agent" class="input small" placeholder="agent（可选）" />
        <input id="task-model" class="input small" placeholder="model（可选）" />`
        }
        <label class="icon-btn composer-attach" for="task-images" aria-label="添加图片" title="添加图片">${clipIcon}</label>
        <input id="task-images" class="visually-hidden" type="file"
               accept="image/png,image/jpeg,image/webp,image/gif" multiple />
        <button id="task-submit" class="composer-send" type="submit"
                aria-label="发送" title="Enter 发送，Ctrl/⌘ + Enter 换行"
                aria-keyshortcuts="Enter">${sendIcon}</button>
      </div>
      <div id="task-form-msg" class="form-msg"></div>
    </form>`;

  const listHeader = (): string => `
    <div class="row hub-toolbar">
      <h2 class="section-title">会话</h2>
      <select id="task-filter" class="input small">
        <option value="">全部状态</option>
        <option value="running">运行中</option>
        <option value="pending">排队中</option>
        <option value="done">已完成</option>
        <option value="failed">失败</option>
        <option value="interrupted">已中断</option>
      </select>
    </div>
    <input id="hub-search" class="input hub-search" placeholder="搜索会话标题或 id" />
    <div id="hub-rows">${skeleton(3)}</div>`;

  const matches = (s: SessionSummary): boolean => {
    const q = query.trim().toLowerCase();
    if (!q) return true;
    return s.title?.toLowerCase().includes(q) || s.id.toLowerCase().includes(q);
  };

  const renderRows = (): string => {
    const latest = latestTasks();
    const list = sessions
      .filter(matches)
      .filter((s) => (status ? latest.get(s.id)?.status === status : true))
      .sort((a, b) => (b.updated_at || b.created_at).localeCompare(a.updated_at || a.created_at));
    if (!list.length) {
      return sessions.length
        ? emptyState('没有匹配的会话', '换个关键词或状态试试。')
        : emptyState('还没有会话', '在上方描述目标并点击「发起」，执行过程会实时出现在这里。');
    }
    return list
      .map((s) => {
        const t = latest.get(s.id);
        return `
          <div class="card session-card hub-row" data-session="${esc(s.id)}">
            <div class="row">
              <span class="session-title">${esc(renderSessionTitle(s))}</span>
              ${t ? chip(taskStatusLabel(t.status), `st-${t.status}`) : ''}
              ${s.model ? chip(s.model, 'dim') : ''}
              <span class="dim time">${fmtTime(s.updated_at || s.created_at)}</span>
              ${t ? `<button class="btn small ghost" data-timeline="${esc(t.id)}" type="button">时间线</button>` : ''}
              ${t && (t.status === 'running' || t.status === 'pending')
                ? `<button class="btn small" data-cancel="${esc(t.id)}" type="button">取消</button>`
                : ''}
            </div>
          </div>`;
      })
      .join('');
  };

  /** Refresh only the list rows so composer input and scroll survive SSE. */
  const reloadRows = async (root: HTMLElement, showError = true): Promise<void> => {
    const box = root.querySelector<HTMLElement>('#hub-rows');
    if (!box) return;
    try {
      const [nextSessions, nextTasks] = await Promise.all([api.listSessions(), api.listTasks()]);
      if (disposed) return;
      sessions = nextSessions;
      tasks = nextTasks;
      box.innerHTML = renderRows();
      syncContinueChip(root);
    } catch (err) {
      if (!disposed && showError) box.innerHTML = errorState(err);
    }
  };

  const syncContinueChip = (root: HTMLElement): void => {
    const el = root.querySelector<HTMLElement>('#composer-continue strong');
    if (el && continueId) el.textContent = continueTitle();
  };

  /** Desktop: the sidebar owns the list, so the page is just "new conversation". */
  const renderComposer = (root: HTMLElement): void => {
    currentId = null;
    liveSessionId = null;
    liveText = '';
    currentMessages = [];
    releaseImages();
    root.classList.remove('content-detail');
    const box = root.querySelector<HTMLElement>('#hub-body');
    if (!box) return;
    box.innerHTML = `
      <div class="welcome">
        <span class="welcome-mark" aria-hidden="true">${brandMarkSvg('welcome')}</span>
        <h1>你好，需要我做什么？</h1>
      </div>
      ${composer()}`;
    mountComposer(root);
    void (async () => {
      try {
        sessions = await api.listSessions();
        syncContinueChip(root);
      } catch {
        // offline: the chip falls back to the id prefix
      }
    })();
  };

  const renderList = (root: HTMLElement): void => {
    currentId = null;
    liveSessionId = null;
    liveText = '';
    currentMessages = [];
    releaseImages();
    root.classList.remove('content-detail');
    const box = root.querySelector<HTMLElement>('#hub-body');
    if (!box) return;
    box.innerHTML = composer() + listHeader();
    mountComposer(root);
    const searchEl = root.querySelector<HTMLInputElement>('#hub-search');
    if (searchEl) searchEl.value = query;
    const statusEl = root.querySelector<HTMLSelectElement>('#task-filter');
    if (statusEl) statusEl.value = status;
    searchEl?.addEventListener('input', () => {
      query = searchEl.value;
      const rows = root.querySelector<HTMLElement>('#hub-rows');
      if (rows) rows.innerHTML = renderRows();
    });
    statusEl?.addEventListener('change', () => {
      status = statusEl.value;
      const rows = root.querySelector<HTMLElement>('#hub-rows');
      if (rows) rows.innerHTML = renderRows();
    });
    void reloadRows(root);
  };

  const showError = (root: HTMLElement, err: unknown): void => {
    const msg = root.querySelector<HTMLElement>('#session-error');
    if (msg) {
      msg.innerHTML = `<span class="err-text">${esc(err instanceof Error ? err.message : String(err))}</span>`;
    }
  };

  /** Put the newest content in view.
   *
   * Desktop docks the composer and scrolls the list itself; phones keep the
   * page scrolling with a sticky composer, so the window must reach its end
   * (the reserved .msg-list padding keeps the last card above the composer). */
  const scrollToEnd = (list: HTMLElement): void => {
    list.scrollTop = list.scrollHeight;
    window.scrollTo({ top: document.documentElement.scrollHeight, behavior: 'auto' });
  };

  /** Re-fetch the open transcript and its task state in place. */
  const refreshDetail = async (root: HTMLElement, id: string): Promise<void> => {
    const list = root.querySelector<HTMLElement>('.msg-list');
    if (!list) return void renderDetail(root, id, false);
    const nearBottom = list.scrollHeight - list.scrollTop - list.clientHeight < 120;
    try {
      const [session, nextTasks, nextApprovals] = await Promise.all([
        api.getSession(id),
        api.listTasks(),
        api.listApprovals(),
      ]);
      if (disposed || currentId !== id) return;
      tasks = nextTasks;
      approvals = nextApprovals;
      currentMessages = session.messages;
      const latest = latestTasks().get(id);
      const meta = root.querySelector<HTMLElement>('#chat-meta');
      if (meta) {
        meta.innerHTML = metaMarkup(session, latest, session.context);
      }
      const running = !!latest && (latest.status === 'running' || latest.status === 'pending');
      const actions = root.querySelector<HTMLElement>('.chat-actions');
      if (running && !root.querySelector('#session-stop') && actions) {
        actions.insertAdjacentHTML('afterbegin', stopBtnMarkup);
      }
      if (!running) root.querySelector('#session-stop')?.remove();
      // The final assistant row is persisted before task.done, so drop the
      // live buffer once the run is terminal.
      if (latest && latest.status !== 'running' && latest.status !== 'pending') {
        liveText = '';
        if (liveTimer !== null) {
          window.clearTimeout(liveTimer);
          liveTimer = null;
        }
      }
      list.innerHTML = transcriptMarkup(session.messages, latest, approvalMarkup(id));
      hydrateImages(root);
      if (nearBottom) scrollToEnd(list);
    } catch {
      // keep the last transcript visible
    }
  };

  /** Resend the last user turn after a failed run. */
  const retryLastTurn = (root: HTMLElement): void => {
    const id = currentId;
    if (!id) return;
    const prompt =
      [...currentMessages].reverse().find((m) => m.role === 'user')?.text?.trim() ?? '';
    if (!prompt) return;
    const btn = root.querySelector<HTMLButtonElement>('[data-retry]');
    if (btn) btn.disabled = true;
    void (async () => {
      try {
        await api.createTask({ prompt, session_id: id, retry: true });
        options.onSessionsChanged?.();
        await refreshDetail(root, id);
      } catch (err) {
        showError(root, err);
        if (btn) btn.disabled = false;
      }
    })();
  };

  /** Resolve one pending ask from the card embedded in the transcript. */
  const decideInline = (root: HTMLElement, btn: HTMLButtonElement): void => {
    const id = btn.dataset.id ?? '';
    const decision = btn.dataset.decide ?? '';
    if (!id || !decision) return;
    btn.disabled = true;
    void (async () => {
      try {
        await api.decideApproval(id, { decision });
      } catch (err) {
        // A ticket resolved elsewhere (double click, another device) is not an
        // error the reader can act on: refresh so the card disappears instead
        // of printing "approval ... not pending".
        if (err instanceof ApiError && (err.status === 404 || err.status === 409)) {
          options.onSessionsChanged?.();
          const session = currentId;
          if (session) await refreshDetail(root, session);
          return;
        }
        btn.disabled = false;
        showError(root, err);
        return;
      }
      options.onSessionsChanged?.();
      const session = currentId;
      if (session) await refreshDetail(root, session);
    })();
  };

  const renderDetail = async (root: HTMLElement, id: string, showSkeleton = true): Promise<void> => {
    currentId = id;
    if (liveSessionId !== id) {
      liveSessionId = id;
      liveText = '';
    }
    root.classList.add('content-detail');
    const box = root.querySelector<HTMLElement>('#hub-body');
    if (!box) return;
    if (showSkeleton) {
      box.innerHTML = `
        <div class="row">
          <button class="btn ghost" id="back-sessions" type="button">← 返回</button>
          <h2 class="section-title">会话详情</h2>
        </div>
        ${skeleton(3)}`;
    }
    try {
      const [session, nextTasks, nextApprovals] = await Promise.all([
        api.getSession(id),
        api.listTasks(),
        api.listApprovals(),
      ]);
      if (disposed) return;
      tasks = nextTasks;
      approvals = nextApprovals;
      const latest = latestTasks().get(id);
      currentMessages = session.messages;
      // Messages typed in the transcript continue this session inline.
      continueId = id;
      box.innerHTML = `
        <div class="row chat-head">
          <button class="icon-btn chat-back" id="back-sessions" type="button"
                  aria-label="返回会话列表" title="返回">${backIcon}</button>
          <h2 class="section-title chat-title">${esc(renderSessionTitle(session))}</h2>
          ${session.model ? chip(session.model, 'dim') : ''}
          <div class="chat-actions">
            ${
              latest && (latest.status === 'running' || latest.status === 'pending')
                ? stopBtnMarkup
                : ''
            }
            ${
              latest
                ? `<button class="icon-btn" id="session-timeline" type="button"
                   aria-label="执行时间线" title="执行时间线">${clockIcon}</button>`
                : ''
            }
            <button class="icon-btn" id="session-trace" type="button"
                    aria-label="耗时归因" title="耗时归因（模型/工具/等人工/编排）">${traceIcon}</button>
            <button class="icon-btn" id="session-rename" type="button"
                    aria-label="重命名" title="重命名">${pencilIcon}</button>
            <button class="icon-btn" id="session-export" type="button"
                    aria-label="导出" title="导出">${downloadIcon}</button>
            <button class="icon-btn danger" id="session-delete" type="button"
                    aria-label="删除会话" title="删除">${trashIcon}</button>
          </div>
        </div>
        <div class="row chat-meta" id="chat-meta">${
          metaMarkup(session, latest, session.context)
        }</div>
        <div class="trace-panel" id="trace-panel" hidden></div>
        <div class="form-msg" id="session-error"></div>
        <div class="msg-list">${transcriptMarkup(session.messages, latest, approvalMarkup(id))}</div>
        <div class="detail-composer">
          ${composer(false, true)}
          <div class="composer-note">AI 生成内容，仅供参考</div>
        </div>`;
      bindDetailActions(root, id, latest);
      mountComposer(root);
      hydrateImages(root);
      const list = root.querySelector<HTMLElement>('.msg-list');
      if (list) scrollToEnd(list);
    } catch (err) {
      if (!disposed) box.innerHTML = errorState(err);
    }
  };

  // Trace viewer (U4): opened per session. Every expand re-reads the spans —
  // a turn that finished since the last look must show up, so no stale cache.
  let traceOpenFor: string | null = null;

  /** One turn's attribution row: totals first, then the span list. */
  const traceTurnMarkup = (turn: TraceTurn, index: number, total: number): string => {
    const s = turn.summary;
    const errs =
      s.errors && Object.keys(s.errors).length
        ? `<span class="chip err">${esc(
            Object.entries(s.errors)
              .map(([k, n]) => `${k}×${n}`)
              .join(' '),
          )}</span>`
        : '';
    const spans = turn.spans
      .map(
        (sp) => `
        <div class="trace-span${sp.status !== 'ok' ? ' trace-span-err' : ''}">
          <span class="trace-kind">${esc(sp.kind)}</span>
          <span class="trace-name">${esc(sp.name || '—')}</span>
          <span class="trace-ms">${fmtDuration(Date.parse(sp.end) - Date.parse(sp.start))}</span>
          ${sp.error_class ? `<span class="chip err">${esc(sp.error_class)}</span>` : ''}
        </div>`,
      )
      .join('');
    return `
      <div class="trace-turn">
        <div class="trace-head">
          <span class="trace-idx">第 ${total - index} 轮</span>
          <span class="dim">${esc(fmtTime(s.start))}</span>
          <strong>${fmtDuration(s.duration_ms)}</strong>
          <span class="dim">模型 ${fmtDuration(s.model_ms)} · 工具 ${fmtDuration(
            s.tool_execution_ms,
          )} · 等人工 ${fmtDuration(s.approval_wait_ms)} · 编排 ${fmtDuration(s.overhead_ms)}</span>
          <span class="dim">${fmtTokens(s.tokens_in)} in / ${fmtTokens(s.tokens_out)} out</span>
          ${s.cost_usd > 0 ? `<span class="dim">${fmtCost(s.cost_usd)}</span>` : ''}
          ${errs}
        </div>
        ${spans}
      </div>`;
  };

  const traceMarkup = (turns: TraceTurn[]): string => {
    if (turns.length === 0) {
      return `<div class="trace-empty dim">暂无 trace 记录。traces 每轮写一条，仅存本机 SQLite。</div>`;
    }
    return turns.map((turn, i) => traceTurnMarkup(turn, i, turns.length)).join('');
  };

  /** Toggle the attribution panel; each expand fetches the current spans (U4). */
  const toggleTrace = async (root: HTMLElement, id: string): Promise<void> => {
    const panel = root.querySelector<HTMLElement>('#trace-panel');
    if (!panel) return;
    if (!panel.hidden && traceOpenFor === id) {
      panel.hidden = true;
      traceOpenFor = null;
      return;
    }
    panel.hidden = false;
    traceOpenFor = id;
    panel.innerHTML = `<div class="trace-empty dim">正在读取 trace…</div>`;
    try {
      const resp = await api.traces(id);
      // A slow reply must not paint over a panel the user already collapsed or
      // switched away from.
      if (disposed || traceOpenFor !== id) return;
      panel.innerHTML = traceMarkup(resp.turns ?? []);
    } catch (err) {
      if (disposed || traceOpenFor !== id) return;
      panel.innerHTML = errorState(err);
    }
  };

  const bindDetailActions = (root: HTMLElement, id: string, latest?: Task): void => {
    root.querySelector('#back-sessions')?.addEventListener('click', () => options.onOpenSession?.(null));
    root.querySelector('#session-timeline')?.addEventListener('click', () => {
      if (latest) options.onOpenTask?.(latest.id);
    });
    root.querySelector('#session-trace')?.addEventListener('click', () => {
      void toggleTrace(root, id);
    });
    root.querySelector('#session-rename')?.addEventListener('click', () => {
      void (async () => {
        const h = root.querySelector<HTMLElement>('#hub-body .section-title');
        const next = await promptDialog({
          title: '重命名会话',
          body: '标题最多 20 个字，留空则恢复为默认标题。',
          value: h?.textContent?.trim() ?? '',
          placeholder: '输入新的会话标题',
          maxLength: 20,
          confirmText: '保存',
        });
        if (next === null) return;
        try {
          await api.renameSession(id, next);
          options.onSessionsChanged?.();
          await renderDetail(root, id, false);
        } catch (err) {
          showError(root, err);
        }
      })();
    });
    root.querySelector('#session-export')?.addEventListener('click', () => {
      void (async () => {
        try {
          downloadBlob(await api.exportSession(id), `chyan-session-${id}.json`);
        } catch (err) {
          showError(root, err);
        }
      })();
    });
    root.querySelector('#session-delete')?.addEventListener('click', () => {
      void (async () => {
        const ok = await confirmDialog({
          title: '删除会话',
          body: '该会话及其消息将被删除，此操作不可撤销。',
          confirmText: '删除',
          danger: true,
        });
        if (!ok) return;
        try {
          await api.deleteSession(id);
          options.onSessionsChanged?.();
          options.onOpenSession?.(null);
        } catch (err) {
          showError(root, err);
        }
      })();
    });
  };

  /** Repaint the pending-image strip after a paste/attach/remove. */
  const renderPending = (root: HTMLElement): void => {
    const box = root.querySelector<HTMLElement>('#composer-images');
    if (!box) return;
    box.hidden = pendingImages.length === 0;
    box.innerHTML = pendingMarkup();
  };

  const addImages = async (root: HTMLElement, files: File[]): Promise<void> => {
    const msg = root.querySelector<HTMLElement>('#task-form-msg');
    if (pendingImages.length + files.length > 4) {
      if (msg) msg.textContent = '最多 4 张图片';
      return;
    }
    for (const file of files) {
      if (file.size > 5 * 1024 * 1024) {
        if (msg) msg.textContent = `${file.name} 超过 5MB`;
        return;
      }
      try {
        pendingImages.push({ name: file.name || 'paste.png', dataUrl: await readDataURL(file) });
      } catch {
        if (msg) msg.textContent = '读取图片失败';
        return;
      }
    }
    if (msg) msg.textContent = '';
    renderPending(root);
  };

  const mountComposer = (root: HTMLElement): void => {
    const promptEl = root.querySelector<HTMLTextAreaElement>('#task-prompt');
    const agentEl = root.querySelector<HTMLInputElement>('#task-agent');
    const modelEl = root.querySelector<HTMLInputElement>('#task-model');
    const workspaceEl = root.querySelector<HTMLInputElement>('#task-workspace');
    const filesEl = root.querySelector<HTMLInputElement>('#task-images');
    const msg = root.querySelector<HTMLElement>('#task-form-msg');
    const form = root.querySelector<HTMLFormElement>('#task-form');
    const submitBtn = root.querySelector<HTMLButtonElement>('#task-submit');

    const draft = loadDraft(HUB_DRAFT);
    if (promptEl) promptEl.value = draft.prompt;
    if (agentEl) agentEl.value = draft.agent;
    if (modelEl) modelEl.value = draft.model;
    if (workspaceEl) workspaceEl.value = draft.workspace;
    const persist = () =>
      saveDraft(HUB_DRAFT, {
        prompt: promptEl?.value ?? '',
        agent: agentEl?.value ?? '',
        workspace: workspaceEl?.value ?? '',
        model: modelEl?.value ?? '',
      });
    promptEl?.addEventListener('input', persist);
    agentEl?.addEventListener('input', persist);
    modelEl?.addEventListener('input', persist);
    workspaceEl?.addEventListener('input', persist);

    filesEl?.addEventListener('change', () => {
      const files = Array.from(filesEl.files ?? []);
      filesEl.value = '';
      if (files.length) void addImages(root, files);
    });

    // Chat-app behavior: paste an image straight into the composer.
    form?.addEventListener('paste', (e) => {
      const items = Array.from((e as ClipboardEvent).clipboardData?.items ?? []);
      const files = items
        .filter((i) => i.kind === 'file' && i.type.startsWith('image/'))
        .map((i) => i.getAsFile())
        .filter((f): f is File => Boolean(f));
      if (!files.length) return;
      e.preventDefault();
      void addImages(root, files);
    });

    root.querySelector('#continue-clear')?.addEventListener('click', () => {
      continueId = null;
      options.onClearContinue?.();
    });

    void (async () => {
      try {
        const info = await api.getWorkspace();
        if (disposed) return;
        const list = root.querySelector<HTMLElement>('#workspace-list');
        if (list) list.innerHTML = info.recent.map((p) => `<option value="${esc(p)}"></option>`).join('');
        if (workspaceEl) {
          workspaceEl.placeholder = '工作区（默认）';
          workspaceEl.title = info.path;
        }
      } catch {
        // offline/daemon without workspace support: keep the plain input
      }
    })();

    // Chat-app convention: Enter sends, Ctrl/⌘/Shift+Enter inserts a newline.
    promptEl?.addEventListener('keydown', (e) => {
      if (e.key !== 'Enter' || e.isComposing) return;
      if (e.ctrlKey || e.metaKey || e.shiftKey) return;
      e.preventDefault();
      form?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    });

    let submitting = false;
    form?.addEventListener('submit', (e) => {
      e.preventDefault();
      if (submitting) return;
      if (!online) {
        if (msg) msg.textContent = 'daemon 离线，无法发起任务';
        return;
      }
      const prompt = promptEl?.value.trim() ?? '';
      const agent = agentEl?.value.trim() ?? '';
      const model = modelEl?.value.trim() ?? '';
      const workspace = workspaceEl?.value.trim() ?? '';
      const images = pendingImages.map((i) => i.dataUrl);
      if (!prompt) return;
      submitting = true;
      if (submitBtn) submitBtn.disabled = true;
      void (async () => {
        try {
          if (images.length > 4) throw new Error('最多 4 张图片');
          const req: CreateTaskRequest = { prompt };
          if (agent) req.agent = agent;
          if (model) req.model = model;
          if (workspace) req.work_dir = workspace;
          if (continueId) req.session_id = continueId;
          if (images.length) req.images = images;
          const task = await api.createTask(req);
          if (msg) msg.textContent = '已提交';
          clearDraft(HUB_DRAFT);
          if (promptEl) promptEl.value = '';
          if (agentEl) agentEl.value = '';
          if (modelEl) modelEl.value = '';
          if (filesEl) filesEl.value = '';
          pendingImages = [];
          renderPending(root);
          if (task.session_id) options.onCreated?.(task.session_id);
          else {
            continueId = null;
            await reloadRows(root);
          }
        } catch (err) {
          if (msg) msg.textContent = `提交失败：${err instanceof Error ? err.message : err}`;
        } finally {
          submitting = false;
          if (submitBtn) submitBtn.disabled = !online;
        }
      })();
    });
  };

  const openViewer = (root: HTMLElement, src: string): void => {
    const overlay = root.querySelector<HTMLElement>('#image-viewer');
    const img = root.querySelector<HTMLImageElement>('#image-viewer-img');
    if (!overlay || !img) return;
    img.src = src;
    overlay.hidden = false;
  };

  const closeViewer = (root: HTMLElement): void => {
    const overlay = root.querySelector<HTMLElement>('#image-viewer');
    if (overlay) overlay.hidden = true;
  };

  const copyText = (text: string): void => {
    if (!text) return;
    if (navigator.clipboard?.writeText) {
      navigator.clipboard.writeText(text).catch(() => legacyCopy(text));
      return;
    }
    legacyCopy(text);
  };

  const flashAction = (btn: HTMLElement): void => {
    btn.classList.add('is-on');
    window.setTimeout(() => btn.classList.remove('is-on'), 1200);
  };

  /** Copy / feedback / read-aloud / share for one assistant reply. */
  const runMessageAction = (btn: HTMLButtonElement, action: string): void => {
    const text =
      btn.closest('.msg')?.querySelector('.msg-text')?.textContent?.trim() ?? '';
    switch (action) {
      case 'copy':
        copyText(text);
        flashAction(btn);
        return;
      case 'like':
      case 'dislike': {
        const row = btn.closest('.msg-actions');
        row?.querySelectorAll('.msg-action.is-on').forEach((el) => {
          if (el !== btn) el.classList.remove('is-on');
        });
        btn.classList.toggle('is-on');
        return;
      }
      case 'speak': {
        const synth = window.speechSynthesis;
        if (!synth || typeof SpeechSynthesisUtterance === 'undefined') return;
        synth.cancel();
        synth.speak(new SpeechSynthesisUtterance(text));
        flashAction(btn);
        return;
      }
      case 'share': {
        if (typeof navigator.share === 'function') {
          void navigator.share({ text }).catch(() => copyText(text));
          return;
        }
        copyText(`${text}\n${location.href}`);
        flashAction(btn);
        return;
      }
    }
  };

  return {
    html,
    mounted(root) {
      rootEl = root;
      root.addEventListener('click', (e) => {
        const target = e.target as HTMLElement;
        const actionBtn = target.closest<HTMLButtonElement>('[data-msg-action]');
        if (actionBtn?.dataset.msgAction) {
          runMessageAction(actionBtn, actionBtn.dataset.msgAction);
          return;
        }
        const decideBtn = target.closest<HTMLButtonElement>('button[data-decide]');
        if (decideBtn) {
          decideInline(root, decideBtn);
          return;
        }
        const chatImage = target.closest<HTMLImageElement>('.msg-image');
        if (chatImage?.src) {
          openViewer(root, chatImage.src);
          return;
        }
        if (target.closest('[data-retry]')) {
          retryLastTurn(root);
          return;
        }
        const stopEl = target.closest<HTMLButtonElement>('#session-stop');
        if (stopEl && currentId) {
          const id = currentId;
          const latest = latestTasks().get(id);
          if (latest) {
            stopEl.disabled = true;
            void (async () => {
              try {
                await api.cancelTask(latest.id);
                options.onSessionsChanged?.();
              } catch (err) {
                showError(root, err);
                stopEl.disabled = false;
                return;
              }
              await refreshDetail(root, id);
            })();
          }
          return;
        }
        const remove = target.closest<HTMLElement>('[data-remove-image]');
        if (remove?.dataset.removeImage !== undefined) {
          pendingImages.splice(Number(remove.dataset.removeImage), 1);
          renderPending(root);
          return;
        }
        const preview = target.closest<HTMLElement>('[data-preview-index]');
        if (preview?.dataset.previewIndex !== undefined) {
          const img = pendingImages[Number(preview.dataset.previewIndex)];
          if (img) openViewer(root, img.dataUrl);
          return;
        }
        const viewer = root.querySelector<HTMLElement>('#image-viewer');
        if (target === viewer || target.closest('#image-viewer-close')) {
          closeViewer(root);
          return;
        }
        const cancel = target.closest<HTMLButtonElement>('[data-cancel]');
        if (cancel?.dataset.cancel) {
          const id = cancel.dataset.cancel;
          cancel.disabled = true;
          void (async () => {
            try {
              await api.cancelTask(id);
              await reloadRows(root);
            } catch (err) {
              cancel.disabled = false;
              const msg = root.querySelector<HTMLElement>('#task-form-msg');
              if (msg) msg.textContent = `取消失败：${err instanceof Error ? err.message : err}`;
            }
          })();
          return;
        }
        const timeline = target.closest<HTMLElement>('[data-timeline]');
        if (timeline?.dataset.timeline) {
          options.onOpenTask?.(timeline.dataset.timeline);
          return;
        }
        const row = target.closest<HTMLElement>('[data-session]');
        if (row?.dataset.session) {
          options.onOpenSession?.(row.dataset.session);
        }
      });
      root.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') closeViewer(root);
      });
      if (currentId) void renderDetail(root, currentId);
      else if (options.showList === false) renderComposer(root);
      else renderList(root);
    },
    refresh(root) {
      // Update the transcript only: rebuilding the detail would wipe the
      // inline composer while the assistant is still streaming.
      if (currentId) {
        void refreshDetail(root, currentId);
      } else if (options.showList !== false) void reloadRows(root);
    },
    onEvent(ev) {
      if (ev.name !== 'task.output') return;
      const data = ev.data as { session_id?: string; text?: string };
      if (!currentId || !data?.session_id || data.session_id !== currentId || !data.text) return;
      liveSessionId = data.session_id;
      liveText += data.text;
      if (rootEl) scheduleLive(rootEl);
    },
    setOnline(next: boolean) {
      online = next;
      const btn = rootEl?.querySelector<HTMLButtonElement>('#task-submit');
      if (btn) btn.disabled = !next;
      const prompt = rootEl?.querySelector<HTMLTextAreaElement>('#task-prompt');
      if (prompt) prompt.readOnly = !next;
      const msg = rootEl?.querySelector<HTMLElement>('#task-form-msg');
      if (msg) msg.textContent = next ? '' : 'daemon 离线，无法发起任务';
    },
    cleanup() {
      disposed = true;
      liveText = '';
      liveSessionId = null;
      if (liveTimer !== null) {
        window.clearTimeout(liveTimer);
        liveTimer = null;
      }
      currentMessages = [];
      releaseImages();
      rootEl?.classList.remove('content-detail');
      rootEl = null;
    },
  };
}

/** Clipboard fallback for plain-http LAN origins, where the async API is off. */
export function legacyCopy(text: string): boolean {
  try {
    const area = document.createElement('textarea');
    area.value = text;
    area.setAttribute('readonly', '');
    area.style.position = 'fixed';
    area.style.top = '0';
    area.style.left = '-9999px';
    document.body.appendChild(area);
    area.select();
    const ok = document.execCommand('copy');
    area.remove();
    return ok;
  } catch {
    return false;
  }
}

/** Read one file as a base64 data URL (PWA image upload). */
export function readDataURL(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(reader.error ?? new Error('读取图片失败'));
    reader.readAsDataURL(file);
  });
}
