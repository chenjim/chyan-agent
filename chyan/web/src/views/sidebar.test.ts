// @vitest-environment happy-dom
import { describe, expect, it } from 'vitest';
import type { SessionSummary, Task } from '../types';
import { groupSessions, latestTaskBySession, renderSidebarSessions } from './sidebar';

function session(over: Partial<SessionSummary> = {}): SessionSummary {
  return {
    id: 's1',
    title: '会话一',
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

function task(over: Partial<Task> = {}): Task {
  return {
    id: 't1',
    session_id: 's1',
    status: 'running',
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

describe('groupSessions', () => {
  const now = new Date('2026-11-02T12:00:00Z');

  it('buckets recent days and older months', () => {
    const groups = groupSessions(
      [
        session({ id: 'a', updated_at: '2026-11-01T00:00:00Z' }),
        session({ id: 'b', updated_at: '2026-10-20T00:00:00Z' }),
        session({ id: 'c', updated_at: '2026-09-15T00:00:00Z' }),
      ],
      now,
    );
    expect(groups.map((g) => g.label)).toEqual(['7 天内', '30 天内', '2026-09']);
    expect(groups[2].items[0].id).toBe('c');
  });

  it('falls back to created_at and tolerates bad dates', () => {
    const groups = groupSessions(
      [
        session({ id: 'a', updated_at: '', created_at: '2026-11-01T00:00:00Z' }),
        session({ id: 'b', updated_at: 'nope', created_at: '' }),
      ],
      now,
    );
    expect(groups[0].label).toBe('7 天内');
    expect(groups[1].label).toBe('更早');
  });
});

describe('latestTaskBySession', () => {
  it('keeps the newest task per session', () => {
    const map = latestTaskBySession([
      task({ id: 'old', created_at: '2024-01-01T00:00:00Z', status: 'done' }),
      task({ id: 'new', created_at: '2024-02-01T00:00:00Z', status: 'running' }),
      task({ id: 'other', session_id: 's2' }),
    ]);
    expect(map.get('s1')?.id).toBe('new');
    expect(map.get('s2')?.id).toBe('other');
  });
});

describe('renderSidebarSessions', () => {
  it('renders grouped rows, marks the active session and live tasks', () => {
    const html = renderSidebarSessions(
      [session({ id: 's1', title: '运行中会话' }), session({ id: 's2', title: '另一个' })],
      [task({ session_id: 's1', status: 'running' })],
      's2',
    );
    expect(html).toContain('data-side-session="s1"');
    expect(html).toContain('side-dot running');
    expect(html).toContain('active');
  });

  it('escapes titles, filters by query and handles empty states', () => {
    const html = renderSidebarSessions([session({ title: '<bc>' })], [], null, 'zzz');
    expect(html).toContain('没有匹配的会话');
    expect(renderSidebarSessions([], [], null)).toContain('还没有会话');
    const escaped = renderSidebarSessions([session({ title: '<bc>' })], [], null);
    expect(escaped).toContain('&lt;bc&gt;');
  });

  it('renders the multi-select bar and checked rows only while selecting', () => {
    const plain = renderSidebarSessions([session({ id: 's1' })], [], 's1');
    expect(plain).not.toContain('side-select-bar');
    expect(plain).not.toContain('side-check');
    expect(plain).toContain('data-side-select');

    const selecting = renderSidebarSessions(
      [session({ id: 's1' }), session({ id: 's2', title: '会话二' })],
      [],
      's1',
      '',
      new Date(),
      { active: true, selected: new Set(['s1']) },
    );
    expect(selecting).toContain('side-select-bar');
    expect(selecting).toContain('已选 1 项');
    expect(selecting).toContain('data-side-delete');
    expect(selecting).toContain('side-session active selecting selected');
    expect(selecting).toContain('aria-pressed="true"');
  });
});
