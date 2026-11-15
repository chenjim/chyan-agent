// App shell: login gate, bottom tab navigation, status bar, SSE wiring.

import './style.css';
import { ApiClient, ApiError } from './api';
import { brandMarkSvg } from './brand';
import { confirmDialog } from './dialog';
import { parseRoute, routeHash, type FileSource } from './nav';
import { openSse, type SseHandle } from './sse';
import { Store, needsViewRefresh, type Tab } from './state';
import { approvalsView } from './views/approvals';
import { cronView } from './views/cron';
import { filesView } from './views/files';
import { loginView } from './views/login';
import { memoryView } from './views/memory';
import { taskDetailView } from './views/task-detail';
import { hubView } from './views/hub';
import type { View } from './render';
import { applyTheme, loadTheme, saveTheme, THEMES, type Theme } from './theme';
import type { SessionSummary, Task } from './types';
import { renderSidebarSessions } from './views/sidebar';
import { createSearchDialog, type SearchDialog } from './search';

const TOKEN_KEY = 'chyan_token';
const SIDEBAR_KEY = 'chyan_sidebar';
type SidebarMode = 'expanded' | 'collapsed' | 'hidden';
const SIDEBAR_CYCLE: Record<SidebarMode, SidebarMode> = {
  expanded: 'collapsed',
  collapsed: 'hidden',
  hidden: 'expanded',
};

// Line icons keep the chrome quiet: no emoji, no icon font, no network fetch.
const ICONS: Record<Tab | 'settings', string> = {
  tasks: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M8.5 6h12M8.5 12h12M8.5 18h12"/><path d="M3.5 6l1.6 1.6L8 4.8"/><path d="M3.5 12l1.6 1.6L8 10.8"/><path d="M3.5 18l1.6 1.6L8 16.8"/></svg>',
  approvals: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3.2l7 3v5c0 4.5-3 7.8-7 9.8-4-2-7-5.3-7-9.8v-5l7-3z"/><path d="M8.8 12l2.1 2.1 4.3-4.4"/></svg>',
  files: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3.2 7A1.8 1.8 0 015 5.2h4.2l2 2H19a1.8 1.8 0 011.8 1.8v8A1.8 1.8 0 0119 18.8H5A1.8 1.8 0 013.2 17V7z"/></svg>',
  cron: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="8.3"/><path d="M12 7.2V12l3.2 2"/></svg>',
  memory: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3.4l1.7 5 4.9 1.6-4.9 1.7-1.7 5-1.7-5L5.4 10l4.9-1.6 1.7-5z"/><path d="M18.2 16.2l.8 2 2 .8-2 .8-.8 2-.8-2-2-.8 2-.8.8-2z"/></svg>',
  settings: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="3.1"/><path d="M12 3.6v2M12 18.4v2M3.6 12h2M18.4 12h2M6.1 6.1l1.4 1.4M16.5 16.5l1.4 1.4M17.9 6.1l-1.4 1.4M7.5 16.5l-1.4 1.4"/></svg>',
};
// 任务与会话是同一件事的两个视角，合并成一个入口。
const TAB_META: Record<Tab, { label: string }> = {
  tasks: { label: '会话' },
  approvals: { label: '审批' },
  files: { label: '文件' },
  cron: { label: '自动化' },
  memory: { label: '记忆' },
};
// Main navigation stays shallow; the less frequent tools live in 设置.
const SETTINGS_TABS: readonly Tab[] = ['files', 'cron', 'memory'];

// Exported for tests; the bootstrap below instantiates it against #app.
export class App {
  private token = localStorage.getItem(TOKEN_KEY) ?? '';
  private tab: Tab = 'tasks';
  private openSessionId: string | null = null;
  private openTaskId: string | null = null;
  private openFilePath: string | null = null;
  private openFileSource: FileSource = 'workspace';
  // Session preselected in the composer after "continue session".
  private pendingSessionId: string | null = null;
  private store = new Store();
  private sse: SseHandle | null = null;
  private reconnectTimer: number | null = null;
  private reconnectDelay = 1000;
  private root: HTMLElement;

  private shellMounted = false;
  private content: HTMLElement | null = null;
  private view: View | null = null;
  private refreshTimer: number | null = null;
  private statusTimer: number | null = null;
  private toastTimer: number | null = null;
  // Daemon reachability: false after an SSE drop/status failure, so the
  // composer never pretends a task can start while the server is gone.
  private online = true;
  private theme: Theme = loadTheme();
  // Sidebar conversation list: fetched independently of the active view so it
  // stays visible while the transcript or another tool is open.
  private sideSessions: SessionSummary[] = [];
  private sideTasks: Task[] = [];
  private sideTimer: number | null = null;
  // Sidebar multi-select: the group icon toggles it, rows then check instead
  // of opening, and the bar's delete action removes the checked sessions.
  private sideSelecting = false;
  private sideSelected = new Set<string>();
  private search: SearchDialog | null = null;
  private sidebar: SidebarMode = 'expanded';

  constructor(root: HTMLElement) {
    this.root = root;
    applyTheme(this.theme);
    const savedSidebar = localStorage.getItem(SIDEBAR_KEY);
    this.sidebar =
      savedSidebar === 'collapsed' || savedSidebar === 'hidden' ? savedSidebar : 'expanded';
    // Follow OS switches only while the preference is "system".
    if (typeof matchMedia === 'function') {
      matchMedia('(prefers-color-scheme: dark)').addEventListener?.('change', () => {
        if (this.theme === 'system') applyTheme('system');
      });
    }
    // Restore the route before the first render: a refresh must reopen the
    // same tab and session instead of resetting to the task list.
    const route = parseRoute(location.hash);
    this.tab = route.tab;
    this.openSessionId = route.sessionId;
    this.openTaskId = route.taskId;
    this.openFilePath = route.filePath;
    this.openFileSource = route.fileSource;
    // Canonicalise legacy hashes (#/tasks) onto the merged #/sessions route.
    this.syncRoute();
    // SSE events only patch the chrome; the active view refreshes in place
    // so composer input and scroll position survive a busy stream.
    this.store.subscribe(() => this.updateChrome());
    window.addEventListener('hashchange', () => this.applyRoute());
    // The settings sheet is a popover: Esc dismisses it like a menu. Listen on
    // the app root so unmounted instances do not leave a window listener.
    this.root.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') this.setSettings(false);
    });
    // Chat-app shortcut: Ctrl/⌘ + J starts a fresh task from anywhere.
    window.addEventListener('keydown', (e) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'j') {
        e.preventDefault();
        this.startNewTask();
      }
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        this.search?.open();
      }
    });
    // Desktop shows the sidebar list; mobile keeps the in-page list.
    if (typeof matchMedia === 'function') {
      matchMedia('(min-width: 900px)').addEventListener?.('change', () => this.renderView());
    }
    if (!this.token) this.renderLogin();
    else this.boot();
  }

  /** Apply a back/forward navigation without touching the history stack. */
  private applyRoute(): void {
    const route = parseRoute(location.hash);
    this.tab = route.tab;
    this.openSessionId = route.sessionId;
    this.openTaskId = route.taskId;
    this.openFilePath = route.filePath;
    this.openFileSource = route.fileSource;
    // Canonicalise legacy hashes (#/tasks) onto the merged #/sessions route.
    this.syncRoute();
    this.setSettings(false);
    this.updateChrome();
    if (this.shellMounted) this.renderView();
  }

  /** Write the current route to the URL (replace: tab switches are not steps). */
  private syncRoute(): void {
    const next = routeHash(this.tab, this.openSessionId, this.openTaskId, this.openFilePath, this.openFileSource);
    if (location.hash === next) return;
    try {
      history.replaceState(null, '', next);
    } catch {
      location.hash = next; // file:// or restricted history: hash fallback
    }
  }

  private boot(): void {
    this.startSse();
    this.renderShell();
    void this.refreshStatus();
    void this.refreshSidebar();
  }

  private startSse(): void {
    this.sse?.close();
    this.sse = openSse({
      token: this.token,
      onEvent: (ev) => {
        // Any traffic proves the stream is healthy: reset the backoff.
        this.reconnectDelay = 1000;
        this.setOnline(true);
        this.store.dispatch(ev);
        this.view?.onEvent?.(ev);
        if (ev.name === 'task.done') this.notifyTask(ev.data);
        // task.output fires per token and changes nothing in the list views;
        // refreshing for each one would thrash the UI. Everything else
        // schedules one coalesced in-place view refresh.
        if (needsViewRefresh(ev.name)) {
          this.scheduleRefresh();
          this.scheduleStatus();
          this.scheduleSidebarRefresh();
        }
      },
      onError: () => {
        // Daemon unreachable: keep the UI usable, but mark it offline.
        this.setOnline(false);
      },
      onClose: () => {
        this.setOnline(false);
        this.scheduleReconnect();
      },
    });
  }

  /** Reconnect after an SSE drop; the remount also re-fetches the active view,
   *  so state recovers by polling even if the stream stays down. */
  private scheduleReconnect(): void {
    if (this.reconnectTimer !== null) return;
    const delay = this.reconnectDelay;
    this.reconnectDelay = Math.min(this.reconnectDelay * 2, 30000);
    this.reconnectTimer = window.setTimeout(() => {
      this.reconnectTimer = null;
      this.startSse();
      void this.refreshStatus();
      this.renderShell();
    }, delay);
  }

  /** Coalesce a burst of SSE events into one refresh of the active view. */
  private scheduleRefresh(): void {
    if (this.refreshTimer !== null) return;
    this.refreshTimer = window.setTimeout(() => {
      this.refreshTimer = null;
      if (this.view?.refresh && this.content) this.view.refresh(this.content);
    }, 250);
  }

  /** Status polling is throttled: pending counts come from the SSE reducer,
   *  the REST status is only needed for the header line. */
  private scheduleStatus(): void {
    if (this.statusTimer !== null) return;
    this.statusTimer = window.setTimeout(() => {
      this.statusTimer = null;
      void this.refreshStatus();
    }, 2000);
  }

  /** Flip the daemon-reachability flag and tell the active view. */
  private setOnline(online: boolean): void {
    if (this.online === online) return;
    this.online = online;
    this.view?.setOnline?.(online);
    this.updateChrome();
  }

  /** Task result cue: in-app toast always, OS notification only when granted
   *  and the page is hidden (no duplicate while the user is watching). */
  private notifyTask(data: unknown): void {
    const d = data as { id?: string; status?: string } | null;
    const label =
      d?.status === 'done'
        ? '任务已完成'
        : d?.status === 'failed'
          ? '任务失败'
          : d?.status === 'interrupted'
            ? '任务已中断'
            : '任务结束';
    this.showToast(label);
    if (document.hidden && this.notificationsGranted()) {
      try {
        new Notification('chyan 呈言智', { body: label, tag: d?.id });
      } catch {
        // Some browsers reject constructor use outside a service worker.
      }
    }
  }

  private notificationsGranted(): boolean {
    return typeof Notification !== 'undefined' && Notification.permission === 'granted';
  }

  /** Explicit opt-in: notifications are requested only from this button. */
  private async requestNotifications(): Promise<void> {
    try {
      const permission = await Notification.requestPermission();
      const btn = this.root.querySelector<HTMLElement>('#notify-toggle');
      if (btn) btn.hidden = permission !== 'default';
      if (permission === 'granted') this.showToast('已开启任务通知');
    } catch {
      // Unsupported/denied: keep the button and never prompt automatically.
    }
  }

  private showToast(text: string): void {
    const el = this.root.querySelector<HTMLElement>('#toast');
    if (!el) return;
    el.textContent = text;
    el.hidden = false;
    if (this.toastTimer !== null) window.clearTimeout(this.toastTimer);
    this.toastTimer = window.setTimeout(() => {
      el.hidden = true;
      this.toastTimer = null;
    }, 4000);
  }

  private async refreshStatus(): Promise<void> {
    const api = this.api();
    try {
      const status = await api.status();
      this.setOnline(true);
      this.store.setState({ status, pendingApprovals: status.pending_approvals });
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        this.logout();
        return;
      }
      this.setOnline(false);
    }
  }

  private api(): ApiClient {
    return new ApiClient({ token: this.token });
  }

  private logout(): void {
    localStorage.removeItem(TOKEN_KEY);
    this.token = '';
    this.sse?.close();
    this.sse = null;
    this.clearTimers();
    this.teardownView();
    this.renderLogin();
  }

  private clearTimers(): void {
    if (this.reconnectTimer !== null) window.clearTimeout(this.reconnectTimer);
    if (this.refreshTimer !== null) window.clearTimeout(this.refreshTimer);
    if (this.statusTimer !== null) window.clearTimeout(this.statusTimer);
    if (this.toastTimer !== null) window.clearTimeout(this.toastTimer);
    if (this.sideTimer !== null) window.clearTimeout(this.sideTimer);
    this.reconnectTimer = null;
    this.refreshTimer = null;
    this.statusTimer = null;
    this.toastTimer = null;
    this.sideTimer = null;
  }

  private teardownView(): void {
    this.view?.cleanup?.();
    this.view = null;
    this.content = null;
    this.shellMounted = false;
  }

  private renderLogin(): void {
    const view = loginView((token) => {
      localStorage.setItem(TOKEN_KEY, token);
      this.token = token;
      this.boot();
    });
    this.root.innerHTML = `<main class="login-shell">${view.html}</main>`;
    view.mounted?.(this.root);
  }

  /** (Re)build the shell once, then re-initialise the active view. Used on
   *  boot, tab switch, reconnect and login — not on SSE events. */
  private renderShell(): void {
    if (!this.shellMounted) this.mountShell();
    this.updateChrome();
    this.renderView();
  }

  private mountShell(): void {
    const toggleIcon =
      '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3.5" y="4.5" width="17" height="15" rx="3"/><path d="M9 4.5v15"/></svg>';
    const searchIcon =
      '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.4"/><path d="M15.8 15.8l4 4"/></svg>';
    const plusIcon =
      '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="8"/><path d="M12 8v8M8 12h8"/></svg>';
    const menuIcon =
      '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 9h14M5 15h14"/></svg>';
    const navItem = (id: Tab, extra = '') => `
      <button class="tab${extra}" data-tab="${id}">
        <span class="tab-icon">${ICONS[id]}${
          id === 'approvals' ? '<span class="badge" data-approval-badge hidden></span>' : ''
        }</span>
        <span class="tab-label">${TAB_META[id].label}</span>
      </button>`;
    const secondary = SETTINGS_TABS.map((id) => navItem(id)).join('');
    this.root.innerHTML = `
      <div class="shell sidebar-${this.sidebar}">
        <aside class="sidebar">
          <header class="topbar">
            <span class="brand-block">
              <span class="brand-text">
                <span class="brand">chyan</span>
                <span class="brand-sub">呈言智 · 个人 Agent 工作台</span>
              </span>
            </span>
            <button class="btn small ghost notify" id="notify-toggle" hidden aria-label="开启任务通知">通知</button>
            <div class="topbar-actions">
              <button class="icon-btn top-search" id="search-open" type="button" aria-label="搜索对话（Ctrl/⌘ + K）">${searchIcon}</button>
              <button class="icon-btn sidebar-toggle" id="sidebar-toggle" type="button" title="收起侧边栏" aria-label="收起侧边栏">${toggleIcon}</button>
            </div>
          </header>
          <div class="rail-logo" aria-hidden="true"><span class="brand-mark">${brandMarkSvg('rail')}</span></div>
          <div class="rail-tools" aria-label="侧边栏快捷操作">
            <button class="icon-btn" id="rail-toggle" type="button" title="隐藏侧边栏">${toggleIcon}</button>
            <button class="icon-btn" id="rail-search" type="button" title="搜索对话">${searchIcon}</button>
            <button class="icon-btn" id="rail-new" type="button" title="新建任务">${plusIcon}</button>
          </div>
          <section class="side-conversations" aria-label="会话列表">
            <button class="side-new" id="new-task" type="button" title="新建任务（Ctrl/⌘ + J）">
              <span class="side-new-icon" aria-hidden="true">+</span>
              <span class="side-new-label">新建任务</span>
              <span class="side-new-kbd">Ctrl J</span>
            </button>
            <div id="side-sessions" class="side-sessions">
              <div class="side-empty">加载中…</div>
            </div>
          </section>
          <nav class="nav-primary" aria-label="主要导航">
            ${navItem('tasks')}
          </nav>
          <div class="settings-pop" id="settings-menu" hidden>
            <div class="settings-title">设置与工具</div>
            ${secondary}
            <div class="settings-sep"></div>
            <div class="settings-title">外观</div>
            <div class="theme-picker" role="group" aria-label="外观主题">
              ${THEMES.map(
                (t) => `<button class="theme-option" data-theme-option="${t.id}"
                        aria-pressed="${t.id === this.theme}">${t.label}</button>`,
              ).join('')}
            </div>
            <div class="settings-sep"></div>
            <div class="settings-about">
              <span>关于</span>
              <span class="mono" id="about-version">—</span>
            </div>
            <div class="settings-sep"></div>
            <button class="tab" id="logout">
              <span class="tab-icon"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M10 5H5.5v14H10"/><path d="M9 12h10M15.5 8.5L19 12l-3.5 3.5"/></svg></span>
              <span class="tab-label">退出登录</span>
            </button>
          </div>
          <button class="tab settings-toggle" id="settings-toggle" data-settings-toggle="1"
                  aria-haspopup="true" aria-expanded="false" aria-controls="settings-menu">
            <span class="tab-icon">${ICONS.settings}</span>
            <span class="tab-label">设置</span>
          </button>
        </aside>
        <button class="shell-menu" id="shell-menu" type="button" title="展开侧边栏" aria-label="展开侧边栏">${menuIcon}</button>
        <main class="content" id="content"></main>
        <div class="search-overlay" id="search-overlay" hidden>
          <div class="search-dialog" role="dialog" aria-modal="true" aria-label="搜索对话">
            <div class="search-bar">
              <span class="search-bar-icon" aria-hidden="true"><svg viewBox="0 0 24 24"><circle cx="11" cy="11" r="6.4"/><path d="M15.8 15.8l4 4"/></svg></span>
              <input id="search-input" class="search-input" placeholder="搜索对话内容…" autocomplete="off" />
              <button class="search-close" id="search-close" type="button" aria-label="关闭搜索">×</button>
            </div>
            <div class="search-results" id="search-results"></div>
          </div>
        </div>
        <div class="toast" id="toast" hidden></div>
        <div class="settings-backdrop" id="settings-backdrop" hidden></div>
      </div>`;
    this.content = this.root.querySelector<HTMLElement>('#content');
    this.shellMounted = true;
    const notifyBtn = this.root.querySelector<HTMLButtonElement>('#notify-toggle');
    if (notifyBtn) {
      notifyBtn.hidden = typeof Notification === 'undefined' || Notification.permission !== 'default';
      notifyBtn.addEventListener('click', () => void this.requestNotifications());
    }
    this.root.querySelector('#settings-toggle')?.addEventListener('click', () => {
      const menu = this.root.querySelector<HTMLElement>('#settings-menu');
      this.setSettings(Boolean(menu?.hidden));
    });
    this.root.querySelector('#settings-backdrop')?.addEventListener('click', () => this.setSettings(false));
    this.root.querySelector('#logout')?.addEventListener('click', () => this.logout());
    this.root.querySelector('#new-task')?.addEventListener('click', () => this.startNewTask());
    // The header icon (desktop + phone) and the collapsed rail open one dialog.
    this.search = createSearchDialog(this.root, this.api(), (id) => this.openSession(id));
    const openSearch = () => this.search?.open();
    this.root.querySelector('#search-open')?.addEventListener('click', openSearch);
    this.root.querySelector('#rail-search')?.addEventListener('click', openSearch);
    this.root.querySelector('#sidebar-toggle')?.addEventListener('click', () => this.setSidebar('collapsed'));
    this.root.querySelector('#rail-toggle')?.addEventListener('click', () =>
      this.setSidebar(SIDEBAR_CYCLE[this.sidebar]),
    );
    this.root.querySelector('#rail-new')?.addEventListener('click', () => this.startNewTask());
    this.root.querySelector('#shell-menu')?.addEventListener('click', () => this.setSidebar('expanded'));
    this.root.addEventListener('click', (e) => {
      const target = e.target as HTMLElement;
      if (target.closest('[data-side-select]')) return this.toggleSideSelection();
      if (target.closest('[data-side-select-all]')) return this.selectAllSideSessions();
      if (target.closest('[data-side-cancel]')) return this.exitSideSelection();
      if (target.closest('[data-side-delete]')) {
        void this.deleteSelectedSessions();
        return;
      }
      const row = target.closest<HTMLElement>('[data-side-session]');
      if (row?.dataset.sideSession) {
        if (this.sideSelecting) this.toggleSideSession(row.dataset.sideSession);
        else this.openSession(row.dataset.sideSession);
      }
    });
    this.root.querySelectorAll<HTMLElement>('[data-theme-option]').forEach((btn) => {
      btn.addEventListener('click', () => {
        const next = btn.dataset.themeOption as Theme;
        this.theme = next;
        saveTheme(next);
        applyTheme(next);
        this.markTheme();
      });
    });
  }

  /** Reflect the stored theme on the segmented control. */
  private markTheme(): void {
    this.root.querySelectorAll<HTMLElement>('[data-theme-option]').forEach((el) => {
      el.setAttribute('aria-pressed', String(el.dataset.themeOption === this.theme));
    });
  }

  /** Show/hide the secondary tools sheet and its click-away layer. */
  private setSettings(open: boolean): void {
    const menu = this.root.querySelector<HTMLElement>('#settings-menu');
    const backdrop = this.root.querySelector<HTMLElement>('#settings-backdrop');
    const toggle = this.root.querySelector<HTMLElement>('#settings-toggle');
    const wasOpen = menu ? !menu.hidden : false;
    if (menu) menu.hidden = !open;
    if (backdrop) backdrop.hidden = !open;
    toggle?.setAttribute('aria-expanded', String(open));
    toggle?.classList.toggle('open', open);
    // Move focus with the sheet so keyboard users land inside it, then hand
    // focus back to the gear when it closes.
    if (open && !wasOpen) menu?.querySelector<HTMLElement>('.tab')?.focus();
    else if (!open && wasOpen) toggle?.focus();
  }

  /** Update topbar/badge/title in place — never rebuild the content area. */
  private updateChrome(): void {
    const st = this.store.getState();
    // Version lives in the settings sheet now that the sidebar status line is gone.
    const about = this.root.querySelector<HTMLElement>('#about-version');
    if (about) about.textContent = st.status ? `v${st.status.version}` : '—';
    const n = st.pendingApprovals;
    this.root.querySelectorAll<HTMLElement>('[data-approval-badge]').forEach((badge) => {
      badge.hidden = n === 0;
      badge.textContent = n > 99 ? '99+' : String(n);
    });
    for (const t of Object.keys(TAB_META) as Tab[]) {
      // Approvals exists twice (desktop footer + mobile bar); mark both.
      this.root
        .querySelectorAll<HTMLElement>(`[data-tab="${t}"]`)
        .forEach((el) => el.classList.toggle('active', t === this.tab));
    }
    // The gear reflects where the user is when they are inside a secondary tool.
    this.root.querySelector<HTMLElement>('#settings-toggle')?.classList.toggle(
      'active',
      (SETTINGS_TABS as readonly string[]).includes(this.tab),
    );
    // Title cue for incoming approvals (visible on phone tab / app switcher).
    document.title =
      st.pendingApprovals > 0 ? `(${st.pendingApprovals}) chyan 呈言智` : 'chyan 呈言智';
  }

  private renderView(): void {
    if (!this.content) return;
    this.view?.cleanup?.();
    const api = this.api();
    this.view =
      this.tab === 'tasks'
        ? this.openTaskId
          ? taskDetailView(api, this.openTaskId, {
              onBack: () => this.openTask(''),
              onOpenSession: (sid) => this.openSession(sid),
            })
          : hubView(api, {
              openSessionId: this.openSessionId,
              continueSessionId: this.pendingSessionId,
              // Desktop already lists sessions in the sidebar; mobile shows them
              // inside the page so the selection stays reachable.
              showList: !this.isDesktop(),
              onSessionsChanged: () => this.scheduleSidebarRefresh(),
              onOpenSession: (id) => this.openSession(id),
              onContinue: (id) => {
                this.pendingSessionId = id;
                this.openSessionId = null;
                this.openTaskId = null;
                this.syncRoute();
                this.updateChrome();
                this.renderView();
                this.renderSidebar();
              },
              onClearContinue: () => {
                this.pendingSessionId = null;
                this.syncRoute();
                this.renderView();
              },
              onOpenTask: (id) => this.openTask(id),
              // After a task is accepted, open its session so the user lands on
              // the transcript instead of staying on the composer.
              onCreated: (sid) => {
                this.pendingSessionId = null;
                this.openSession(sid);
              },
            })
        : this.tab === 'approvals'
          ? approvalsView(api)
          : this.tab === 'cron'
            ? cronView(api, {
                onOpenSession: (sid) => this.openSession(sid),
              })
            : this.tab === 'memory'
              ? memoryView(api)
              : filesView(api, this.openFilePath ?? '', Boolean(this.openFilePath),
                  this.openFileSource === 'artifacts' ? 'artifacts' : '');
    this.content.innerHTML = this.view.html;
    this.view.mounted?.(this.content);
    this.view.setOnline?.(this.online);
  }

  /** Open (or close) one task's execution timeline and keep the URL in sync. */
  private openTask(id: string): void {
    this.openSessionId = null;
    this.openTaskId = id || null;
    this.syncRoute();
    this.updateChrome();
    this.renderView();
    this.renderSidebar();
  }

  /** Open (or close) one session's transcript inside the merged hub. */
  private openSession(id: string | null): void {
    this.openSessionId = id;
    this.openTaskId = null;
    this.tab = 'tasks';
    this.syncRoute();
    this.updateChrome();
    this.renderView();
    this.renderSidebar();
    // The sidebar list mirrors session state (new titles, deleted rows).
    this.scheduleSidebarRefresh();
  }

  /** Start a fresh task: clear the conversation selection and focus the input. */
  private startNewTask(): void {
    this.pendingSessionId = null;
    this.openSession(null);
    this.root.querySelector<HTMLTextAreaElement>('#task-prompt')?.focus();
  }

  /** Persist and apply one of the three sidebar states. */
  private setSidebar(mode: SidebarMode): void {
    this.sidebar = mode;
    localStorage.setItem(SIDEBAR_KEY, mode);
    this.applySidebar();
  }

  private applySidebar(): void {
    const shell = this.root.querySelector<HTMLElement>('.shell');
    if (!shell) return;
    shell.classList.remove('sidebar-expanded', 'sidebar-collapsed', 'sidebar-hidden');
    shell.classList.add(`sidebar-${this.sidebar}`);
    this.root
      .querySelector('#sidebar-toggle')
      ?.setAttribute('title', this.sidebar === 'expanded' ? '收起侧边栏' : '展开侧边栏');
    this.root
      .querySelector('#rail-toggle')
      ?.setAttribute('title', this.sidebar === 'collapsed' ? '隐藏侧边栏' : '展开侧边栏');
  }

  /** Render the sidebar conversation list from the cached daemon data. */
  private renderSidebar(): void {
    const box = this.root.querySelector<HTMLElement>('#side-sessions');
    if (!box) return;
    box.innerHTML = renderSidebarSessions(
      this.sideSessions,
      this.sideTasks,
      this.openSessionId,
      '',
      new Date(),
      { active: this.sideSelecting, selected: this.sideSelected },
    );
  }

  /** Group icon: toggle multi-select mode (and clear any stale checks). */
  private toggleSideSelection(): void {
    this.sideSelecting = !this.sideSelecting;
    this.sideSelected.clear();
    this.renderSidebar();
  }

  private exitSideSelection(): void {
    this.sideSelecting = false;
    this.sideSelected.clear();
    this.renderSidebar();
  }

  private selectAllSideSessions(): void {
    this.sideSelected = new Set(this.sideSessions.map((s) => s.id));
    this.renderSidebar();
  }

  private toggleSideSession(id: string): void {
    if (this.sideSelected.has(id)) this.sideSelected.delete(id);
    else this.sideSelected.add(id);
    this.renderSidebar();
  }

  /** Delete every checked session, then leave selection mode and refresh. */
  private async deleteSelectedSessions(): Promise<void> {
    const ids = [...this.sideSelected];
    if (!ids.length) return;
    const ok = await confirmDialog({
      title: '删除会话',
      body: `将删除选中的 ${ids.length} 个会话及其消息，此操作不可撤销。`,
      confirmText: '删除',
      danger: true,
    });
    if (!ok) return;
    const results = await Promise.allSettled(ids.map((id) => this.api().deleteSession(id)));
    const failed = results.filter((r) => r.status === 'rejected').length;
    this.sideSelecting = false;
    this.sideSelected.clear();
    if (this.openSessionId && ids.includes(this.openSessionId)) this.openSession(null);
    else this.renderSidebar();
    await this.refreshSidebar();
    this.showToast(failed ? `已删除 ${ids.length - failed} 个会话，${failed} 个失败` : `已删除 ${ids.length} 个会话`);
  }

  /** Refresh the sidebar list (coalesced; SSE bursts hit the daemon once). */
  private scheduleSidebarRefresh(): void {
    if (this.sideTimer !== null) return;
    this.sideTimer = window.setTimeout(() => {
      this.sideTimer = null;
      void this.refreshSidebar();
    }, 300);
  }

  private async refreshSidebar(): Promise<void> {
    try {
      const [sessions, tasks] = await Promise.all([this.api().listSessions(), this.api().listTasks()]);
      this.sideSessions = sessions;
      this.sideTasks = tasks;
      // Drop checks for sessions that disappeared elsewhere (SSE refresh or
      // another client); otherwise the count and bulk delete target stale ids.
      const live = new Set(sessions.map((s) => s.id));
      for (const id of this.sideSelected) {
        if (!live.has(id)) this.sideSelected.delete(id);
      }
      this.renderSidebar();
    } catch {
      // Offline: keep the last known list rather than blanking the sidebar.
    }
  }

  /** Mobile keeps the list in the page; desktop uses the sidebar list. */
  private isDesktop(): boolean {
    return typeof matchMedia === 'function' && matchMedia('(min-width: 900px)').matches;
  }

  handleClick(e: Event): void {
    const el = (e.target as HTMLElement).closest<HTMLElement>('[data-tab]');
    if (el?.dataset.tab) {
      this.tab = el.dataset.tab as Tab;
      this.setSettings(false);
      this.openSessionId = null;
      this.openTaskId = null;
      this.openFilePath = null;
      this.openFileSource = 'workspace';
      this.pendingSessionId = null;
      this.syncRoute();
      this.updateChrome();
      this.renderView();
      this.renderSidebar();
    }
  }
}

const rootEl = document.querySelector<HTMLElement>('#app');
if (rootEl) {
  const app = new App(rootEl);
  // Bottom tab bar is persistent; delegate clicks on the app root.
  rootEl.addEventListener('click', (e) => app.handleClick(e));
}

// Register the service worker for PWA install/offline shell.
if ('serviceWorker' in navigator && location.protocol !== 'file:') {
  window.addEventListener('load', () => {
    void navigator.serviceWorker.register(new URL('sw.js', document.baseURI)).catch(() => {});
  });
}
