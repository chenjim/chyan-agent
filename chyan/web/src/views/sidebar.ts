// Sidebar conversation list, grouped like a chat app: recent days first, then
// month buckets. It is a navigation surface, so every row opens a transcript.

import { esc } from '../format';
import type { SessionSummary, Task } from '../types';

export interface SessionGroup {
  label: string;
  items: SessionSummary[];
}

const DAY = 24 * 60 * 60 * 1000;

/** Group sessions by recency; deterministic when `now` is injected (tests). */
export function groupSessions(
  sessions: SessionSummary[],
  now: Date = new Date(),
): SessionGroup[] {
  const stampOf = (s: SessionSummary): number => {
    const t = Date.parse(s.updated_at || s.created_at);
    return Number.isNaN(t) ? 0 : t;
  };
  const sorted = [...sessions].sort((a, b) => stampOf(b) - stampOf(a));
  const groups = new Map<string, SessionSummary[]>();
  const push = (label: string, s: SessionSummary) => {
    const list = groups.get(label) ?? [];
    list.push(s);
    groups.set(label, list);
  };
  for (const s of sorted) {
    const stamp = Date.parse(s.updated_at || s.created_at);
    if (Number.isNaN(stamp)) {
      push('更早', s);
      continue;
    }
    const age = now.getTime() - stamp;
    if (age <= 7 * DAY) push('7 天内', s);
    else if (age <= 30 * DAY) push('30 天内', s);
    else {
      const d = new Date(stamp);
      push(`${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}`, s);
    }
  }
  return Array.from(groups, ([label, items]) => ({ label, items }));
}

/** Latest task per session, used for the live dot and row tooltips. */
export function latestTaskBySession(tasks: Task[]): Map<string, Task> {
  const out = new Map<string, Task>();
  for (const t of tasks) {
    const prev = out.get(t.session_id);
    if (!prev || t.created_at > prev.created_at) out.set(t.session_id, t);
  }
  return out;
}

export interface SidebarSelection {
  /** Multi-select mode is on: rows toggle instead of navigating. */
  active: boolean;
  selected: ReadonlySet<string>;
}

const SELECT_ICON =
  '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h10M18 7h2M4 17h2M10 17h10"/><circle cx="16" cy="7" r="2"/><circle cx="8" cy="17" r="2"/></svg>';
const CHECK_ICON = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12.5l4.2 4.3L19 6.8"/></svg>';

export function renderSidebarSessions(
  sessions: SessionSummary[],
  tasks: Task[],
  activeId: string | null,
  query = '',
  now: Date = new Date(),
  select?: SidebarSelection,
): string {
  const q = query.trim().toLowerCase();
  const filtered = q
    ? sessions.filter(
        (s) => s.title?.toLowerCase().includes(q) || s.id.toLowerCase().includes(q),
      )
    : sessions;
  if (!filtered.length) {
    return `<div class="side-empty">${sessions.length ? '没有匹配的会话' : '还没有会话'}</div>`;
  }
  const latest = latestTaskBySession(tasks);
  const selecting = Boolean(select?.active);
  const selected = select?.selected ?? new Set<string>();
  const bar = selecting
    ? `
      <div class="side-select-bar">
        <span class="side-select-count">已选 ${selected.size} 项</span>
        <button class="side-select-action" type="button" data-side-select-all>全选</button>
        <button class="side-select-action" type="button" data-side-cancel>取消</button>
        <button class="side-select-action danger" type="button" data-side-delete
                ${selected.size ? '' : 'disabled'}>删除</button>
      </div>`
    : '';
  return (
    bar +
    groupSessions(filtered, now)
      .map(
        (g) => `
      <div class="side-group">
        <div class="side-group-label">
          <span>${esc(g.label)}</span>
          <button class="side-group-select${selecting ? ' active' : ''}" type="button"
                  data-side-select aria-pressed="${selecting}"
                  title="${selecting ? '退出多选' : '多选删除会话'}"
                  aria-label="${selecting ? '退出多选' : '多选删除会话'}">${SELECT_ICON}</button>
        </div>
        ${g.items
          .map((s) => {
            const t = latest.get(s.id);
            const live = t && (t.status === 'running' || t.status === 'pending');
            const checked = selecting && selected.has(s.id);
            return `
            <button class="side-session${s.id === activeId ? ' active' : ''}${
              selecting ? ' selecting' : ''
            }${checked ? ' selected' : ''}"
                    type="button" data-side-session="${esc(s.id)}"
                    ${selecting ? `aria-pressed="${checked}"` : ''}
                    title="${esc(s.title || s.id)}">
              ${selecting ? `<span class="side-check" aria-hidden="true">${CHECK_ICON}</span>` : ''}
              <span class="side-session-title">${esc(s.title || s.id.slice(0, 8))}</span>
              ${live ? `<span class="side-dot ${esc(t.status)}" aria-label="${esc(t.status)}"></span>` : ''}
            </button>`;
          })
          .join('')}
      </div>`,
      )
      .join('')
  );
}
