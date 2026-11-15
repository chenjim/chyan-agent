// Per-composer draft persistence. A refresh must not lose typed input,
// including an optionally chosen agent (FR-12 W1). Drafts are namespaced by
// scope so a future session composer can keep its own entry.

export interface ComposerDraft {
  prompt: string;
  agent: string;
  workspace: string;
  model: string;
}

const EMPTY: ComposerDraft = { prompt: '', agent: '', workspace: '', model: '' };

const PREFIX = 'chyan_draft:';

function storage(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    return null; // private mode / blocked storage: drafts simply stay in memory
  }
}

export function loadDraft(scope: string): ComposerDraft {
  const s = storage();
  if (!s) return { ...EMPTY };
  const raw = s.getItem(PREFIX + scope);
  if (!raw) return { ...EMPTY };
  try {
    const parsed = JSON.parse(raw) as Partial<ComposerDraft>;
    const str = (v: unknown) => (typeof v === 'string' ? v : '');
    return {
      prompt: str(parsed.prompt),
      agent: str(parsed.agent),
      workspace: str(parsed.workspace),
      model: str(parsed.model),
    };
  } catch {
    s.removeItem(PREFIX + scope); // corrupt entry must not break the composer
    return { ...EMPTY };
  }
}

export function saveDraft(scope: string, draft: ComposerDraft): void {
  const s = storage();
  if (!s) return;
  if (!draft.prompt && !draft.agent && !draft.workspace && !draft.model) {
    s.removeItem(PREFIX + scope);
    return;
  }
  try {
    s.setItem(PREFIX + scope, JSON.stringify(draft));
  } catch {
    // Quota or blocked storage: keep the in-page value instead of failing.
  }
}

export function clearDraft(scope: string): void {
  storage()?.removeItem(PREFIX + scope);
}
