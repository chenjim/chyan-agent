import { describe, expect, it } from 'vitest';
import { parseRoute, routeHash } from './nav';

const blank = { tab: 'tasks', sessionId: null, taskId: null, filePath: null, fileSource: 'workspace' };

describe('parseRoute', () => {
  it('defaults to the task list without a route', () => {
    expect(parseRoute('')).toEqual(blank);
    expect(parseRoute('#')).toEqual(blank);
  });

  it('parses known tabs', () => {
    expect(parseRoute('#/approvals')).toEqual({ ...blank, tab: 'approvals' });
    expect(parseRoute('#/files')).toEqual({ ...blank, tab: 'files' });
  });

  it('falls back to tasks for an unknown tab', () => {
    expect(parseRoute('#/nope')).toEqual(blank);
  });

  it('keeps detail ids on the merged conversation tab', () => {
    // #/sessions/<id> is the legacy path for a session transcript.
    expect(parseRoute('#/sessions/sess-1')).toEqual({ ...blank, sessionId: 'sess-1' });
    expect(parseRoute('#/sessions')).toEqual(blank);
    expect(parseRoute('#/tasks/tsk-1')).toEqual({ ...blank, taskId: 'tsk-1' });
    expect(parseRoute('#/files/src/a.ts')).toEqual({ ...blank, tab: 'files', filePath: 'src/a.ts' });
    expect(parseRoute('#/artifacts/abs/out.txt')).toEqual({
      ...blank, tab: 'files', filePath: 'abs/out.txt', fileSource: 'artifacts',
    });
  });

  it('decodes an escaped id and tolerates a bad escape', () => {
    expect(parseRoute('#/sessions/a%20b')).toEqual({ ...blank, sessionId: 'a b' });
    expect(parseRoute('#/sessions/%zz')).toEqual({ ...blank, sessionId: '%zz' });
  });
});

describe('routeHash', () => {
  it('round-trips detail routes', () => {
    expect(parseRoute(routeHash('tasks', 'a b')).sessionId).toBe('a b');
    expect(routeHash('tasks', 'a b')).toBe('#/sessions/a%20b');
    expect(parseRoute(routeHash('tasks', null, 't1')).taskId).toBe('t1');
    expect(parseRoute(routeHash('files', null, null, 'src/a.ts')).filePath).toBe('src/a.ts');
    const artifact = parseRoute(routeHash('files', null, null, 'out.txt', 'artifacts'));
    expect(artifact.fileSource).toBe('artifacts');
    expect(artifact.filePath).toBe('out.txt');
  });

  it('omits ids on plain tabs', () => {
    expect(routeHash('tasks', null)).toBe('#/sessions');
    expect(routeHash('approvals', null)).toBe('#/approvals');
    expect(routeHash('files', null, null, null)).toBe('#/files');
  });

  it('treats legacy #/tasks as the merged conversation list', () => {
    expect(parseRoute('#/tasks')).toEqual(blank);
  });
});
