// Tests for state reducer and store.

import { describe, expect, it, vi } from 'vitest';
import { applyEvent, eventLine, needsViewRefresh, Store, type AppState } from './state';

const base = (): AppState => ({ status: null, pendingApprovals: 0, taskEvents: [] });

describe('applyEvent', () => {
  it('increments pending on approval.requested', () => {
    const next = applyEvent(base(), { name: 'approval.requested', data: { id: 'a1' } });
    expect(next.pendingApprovals).toBe(1);
  });

  it('decrements pending on approval.resolved, clamped at zero', () => {
    const next = applyEvent(base(), { name: 'approval.resolved', data: { id: 'a1' } });
    expect(next.pendingApprovals).toBe(0);
  });

  it('keeps the same object for unknown events', () => {
    const s = base();
    expect(applyEvent(s, { name: 'unknown.thing', data: null })).toBe(s);
  });

  it('records task event lines', () => {
    const next = applyEvent(base(), { name: 'task.started', data: { id: 't1' } });
    expect(next.taskEvents[0]).toContain('t1');
  });

  it('caps the event log at 50 entries', () => {
    let s = base();
    for (let i = 0; i < 60; i++) {
      s = applyEvent(s, { name: 'task.started', data: { id: `t${i}` } });
    }
    expect(s.taskEvents.length).toBe(50);
    expect(s.taskEvents[0]).toContain('t59');
  });
});

describe('eventLine', () => {
  it('formats all known events', () => {
    expect(eventLine({ name: 'task.done', data: { id: 't1' } })).toContain('t1');
    expect(eventLine({ name: 'task.output', data: { id: 't1', text: 'hi' } })).toContain('hi');
    expect(eventLine({ name: 'approval.requested', data: { id: 'a1' } })).toContain('a1');
    expect(eventLine({ name: 'weird', data: null })).toBeNull();
  });
});

describe('needsViewRefresh', () => {
  it('skips per-token output but refreshes on list-changing events', () => {
    expect(needsViewRefresh('task.output')).toBe(false);
    expect(needsViewRefresh('task.started')).toBe(true);
    expect(needsViewRefresh('task.done')).toBe(true);
    expect(needsViewRefresh('approval.requested')).toBe(true);
    expect(needsViewRefresh('approval.resolved')).toBe(true);
  });
});

describe('Store', () => {
  it('notifies subscribers on dispatch and setState', () => {
    const store = new Store();
    const fn = vi.fn();
    const unsub = store.subscribe(fn);
    store.dispatch({ name: 'approval.requested', data: { id: 'a1' } });
    store.setState({ pendingApprovals: 5 });
    expect(fn).toHaveBeenCalledTimes(2);
    unsub();
    store.dispatch({ name: 'approval.resolved', data: { id: 'a1' } });
    expect(fn).toHaveBeenCalledTimes(2);
    expect(store.getState().pendingApprovals).toBe(4);
  });
});
