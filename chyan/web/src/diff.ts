// Diff line classification and HTML rendering for approval previews.
// The renderer escapes every piece of raw text; only the wrapper markup is fixed.

export type DiffLineKind = 'add' | 'del' | 'hunk' | 'meta' | 'ctx';

export interface DiffLine {
  kind: DiffLineKind;
  text: string;
}

/** Classify one unified-diff line by its leading marker. */
export function classifyDiffLine(raw: string): DiffLine {
  if (raw.startsWith('+++') || raw.startsWith('---') || raw.startsWith('diff ') || raw.startsWith('index ')) {
    return { kind: 'meta', text: raw };
  }
  if (raw.startsWith('@@')) return { kind: 'hunk', text: raw };
  if (raw.startsWith('+')) return { kind: 'add', text: raw };
  if (raw.startsWith('-')) return { kind: 'del', text: raw };
  return { kind: 'ctx', text: raw };
}

export function splitDiff(diff: string): DiffLine[] {
  return diff.split(/\r?\n/).map(classifyDiffLine);
}

/**
 * Render a unified diff to safe HTML. `esc` must HTML-escape raw text.
 * Returns '<div class="diff-empty">无 diff 预览</div>' for empty input.
 */
export function renderDiffHtml(diff: string, esc: (s: string) => string): string {
  if (!diff) return '<div class="diff-empty">无 diff 预览</div>';
  const lines = splitDiff(diff)
    .map((l) => `<div class="diff-line diff-${l.kind}">${esc(l.text) || '&nbsp;'}</div>`)
    .join('');
  return `<div class="diff-view">${lines}</div>`;
}
