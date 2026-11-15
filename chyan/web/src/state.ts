// Central app state + SSE wiring. Pure reducer applies SSE event payloads;
// subscribe() notifies views to re-render (mobile-sized data, full re-render is fine).

import type { ApprovalTicket, SseEventName, Status, Task } from './types';

export type Tab = 'tasks' | 'approvals' | 'files' | 'cron' | 'memory';

export interface AppState {
  status: Status | null;
  pendingApprovals: number;
  taskEvents: string[]; // recent human-readable event lines
}

export type SseEvent = { name: SseEventName | string; data: unknown };

/** Apply one SSE event to the state (pure, returns the same object if nothing changed). */
export function applyEvent(state: AppState, ev: SseEvent): AppState {
  const line = eventLine(ev);
  const taskEvents = line ? [line, ...state.taskEvents].slice(0, 50) : state.taskEvents;
  let { pendingApprovals } = state;
  switch (ev.name) {
    case 'approval.requested':
      pendingApprovals += 1;
      break;
    case 'approval.resolved':
      pendingApprovals = Math.max(0, pendingApprovals - 1);
      break;
    default:
      break;
  }
  if (taskEvents === state.taskEvents && pendingApprovals === state.pendingApprovals) return state;
  return { ...state, pendingApprovals, taskEvents };
}

/** Human-readable one-liner for an SSE event payload (best effort). */
export function eventLine(ev: SseEvent): string | null {
  const d = ev.data as Record<string, unknown> | string | null;
  const id = typeof d === 'object' && d !== null ? String(d.id ?? d.task_id ?? '') : '';
  switch (ev.name) {
    case 'task.started':
      return `任务 ${id} 开始`;
    case 'task.output':
      return typeof d === 'object' && d !== null && typeof d.text === 'string'
        ? `任务 ${id}: ${d.text.slice(0, 80)}`
        : `任务 ${id} 输出`;
    case 'task.done':
      return `任务 ${id} 结束`;
    case 'approval.requested':
      return `新审批 ${id}`;
    case 'approval.resolved':
      return `审批 ${id} 已处理`;
    case 'cron.run':
      return `定时任务 ${typeof d === 'object' && d !== null ? String(d.name ?? '') : ''} 已执行`;
    default:
      return null;
  }
}

/** SSE events that change list content and warrant an in-place view refresh.
 *  task.output arrives per token and only affects transcripts, so it is
 *  skipped: refreshing lists for every token would thrash the UI. */
export function needsViewRefresh(name: string): boolean {
  return name !== 'task.output';
}

export type Listener = (state: AppState) => void;

export class Store {
  private state: AppState = { status: null, pendingApprovals: 0, taskEvents: [] };
  private listeners = new Set<Listener>();

  getState(): AppState {
    return this.state;
  }

  setState(patch: Partial<AppState>): void {
    this.state = { ...this.state, ...patch };
    this.emit();
  }

  dispatch(ev: SseEvent): void {
    const next = applyEvent(this.state, ev);
    if (next !== this.state) {
      this.state = next;
      this.emit();
    }
  }

  subscribe(fn: Listener): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  private emit(): void {
    for (const fn of this.listeners) fn(this.state);
  }
}

// Convenience payload shapes used by views when refreshing after an SSE event.
export function isTaskPayload(d: unknown): d is Task {
  return typeof d === 'object' && d !== null && 'id' in d && 'status' in d;
}

export function isApprovalPayload(d: unknown): d is ApprovalTicket {
  return typeof d === 'object' && d !== null && 'id' in d && 'action' in d;
}
