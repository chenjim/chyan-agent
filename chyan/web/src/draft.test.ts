// @vitest-environment happy-dom
import { beforeEach, describe, expect, it } from 'vitest';
import { clearDraft, loadDraft, saveDraft } from './draft';

describe('draft persistence', () => {
  beforeEach(() => localStorage.clear());

  const empty = { prompt: '', agent: '', workspace: '', model: '' };

  it('round-trips a composer draft', () => {
    saveDraft('task', { prompt: 'hello', agent: 'reviewer', workspace: '/repo', model: 'm1' });
    expect(loadDraft('task')).toEqual({ prompt: 'hello', agent: 'reviewer', workspace: '/repo', model: 'm1' });
  });

  it('returns an empty draft when nothing is stored', () => {
    expect(loadDraft('task')).toEqual(empty);
  });

  it('drops an empty draft instead of storing it', () => {
    saveDraft('task', { prompt: 'x', agent: '', workspace: '', model: '' });
    saveDraft('task', empty);
    expect(localStorage.getItem('chyan_draft:task')).toBeNull();
  });

  it('recovers from corrupt JSON', () => {
    localStorage.setItem('chyan_draft:task', '{bad');
    expect(loadDraft('task')).toEqual(empty);
    expect(localStorage.getItem('chyan_draft:task')).toBeNull();
  });

  it('ignores non-string fields and clears explicitly', () => {
    localStorage.setItem('chyan_draft:task', JSON.stringify({ prompt: 42, agent: null }));
    expect(loadDraft('task')).toEqual(empty);
    saveDraft('task', { prompt: 'keep', agent: '', workspace: '', model: '' });
    clearDraft('task');
    expect(loadDraft('task')).toEqual(empty);
  });
});
