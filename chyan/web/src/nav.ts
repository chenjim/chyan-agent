// URL routing for the workbench shell. The route lives in the hash so the
// PWA works under any base path and a browser refresh restores the same tab
// and open session (FR-12).

import type { Tab } from './state';

// "tasks" is the merged conversation tab (composer + sessions + timelines).
export const TABS: readonly Tab[] = ['tasks', 'approvals', 'files', 'cron', 'memory'];

export type FileSource = 'workspace' | 'artifacts';

export interface Route {
  tab: Tab;
  sessionId: string | null;
  taskId: string | null;
  filePath: string | null;
  fileSource: FileSource;
}

function decodeId(raw: string): string | null {
  if (!raw) return null;
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw; // malformed escape: keep the raw id rather than drop it
  }
}

/** Parse location.hash; unknown or empty routes fall back to the conversation list. */
export function parseRoute(hash: string): Route {
  const path = hash.replace(/^#\/?/, '');
  const [rawTab, ...rest] = path.split('/');
  const joined = decodeId(rest.join('/'));
  if (rawTab === 'artifacts') {
    return { tab: 'files', sessionId: null, taskId: null, filePath: joined, fileSource: 'artifacts' };
  }
  // One namespace for the merged tab: #/sessions lists conversations and
  // #/sessions/<id> opens one transcript.
  if (rawTab === 'sessions') {
    return { tab: 'tasks', sessionId: joined, taskId: null, filePath: null, fileSource: 'workspace' };
  }
  const tab = TABS.includes(rawTab as Tab) ? (rawTab as Tab) : 'tasks';
  return {
    tab,
    sessionId: null,
    taskId: tab === 'tasks' ? joined : null,
    filePath: tab === 'files' ? joined : null,
    fileSource: 'workspace',
  };
}

/** Build the canonical hash for a route (detail ids ride on their tab). */
export function routeHash(
  tab: Tab,
  sessionId: string | null,
  taskId: string | null = null,
  filePath: string | null = null,
  fileSource: FileSource = 'workspace',
): string {
  if (tab === 'tasks' && sessionId) {
    return `#/sessions/${encodeURIComponent(sessionId)}`;
  }
  if (tab === 'tasks' && taskId) {
    return `#/tasks/${encodeURIComponent(taskId)}`;
  }
  if (tab === 'files' && filePath) {
    const prefix = fileSource === 'artifacts' ? 'artifacts' : 'files';
    return `#/${prefix}/${encodeURIComponent(filePath)}`;
  }
  // The merged conversation tab lives under /sessions even when no id is open.
  if (tab === 'tasks') return '#/sessions';
  return `#/${tab}`;
}
