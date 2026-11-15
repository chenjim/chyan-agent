// Custom prompt/confirm overlays. Native window.prompt/confirm are blocked
// in sandboxed hosts (Trae's embedded browser, installed PWAs), so views use
// these in-page dialogs instead.

import { esc } from './format';

export interface PromptDialogOptions {
  title: string;
  /** Secondary line under the title. */
  body?: string;
  /** Initial input value. */
  value?: string;
  placeholder?: string;
  maxLength?: number;
  confirmText?: string;
  cancelText?: string;
}

export interface ConfirmDialogOptions {
  title: string;
  body?: string;
  confirmText?: string;
  cancelText?: string;
  /** Paint the confirm button as destructive. */
  danger?: boolean;
}

interface DialogOptions {
  title: string;
  body?: string;
  input?: PromptDialogOptions;
  confirmText?: string;
  cancelText?: string;
  danger?: boolean;
}

/** Closes the dialog currently on screen, if any. */
let activeClose: ((value: string | null) => void) | null = null;

function openDialog(options: DialogOptions): Promise<string | null> {
  // Only one dialog at a time: a second open dismisses the first as a cancel.
  activeClose?.(null);
  return new Promise((resolve) => {
    const previous =
      document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const overlay = document.createElement('div');
    overlay.className = 'dialog-overlay';
    overlay.innerHTML = `
      <div class="dialog" role="dialog" aria-modal="true" aria-labelledby="dialog-title">
        <h3 class="dialog-title" id="dialog-title">${esc(options.title)}</h3>
        ${options.body ? `<p class="dialog-body">${esc(options.body)}</p>` : ''}
        ${
          options.input
            ? `<input class="dialog-input" type="text" value="${esc(options.input.value ?? '')}"
                 placeholder="${esc(options.input.placeholder ?? '')}"
                 ${options.input.maxLength ? `maxlength="${options.input.maxLength}"` : ''}
                 aria-label="${esc(options.title)}" />`
            : ''
        }
        <div class="dialog-actions">
          <button class="btn ghost" type="button" data-dialog-cancel>${esc(options.cancelText ?? '取消')}</button>
          <button class="btn ${options.danger ? 'danger' : 'primary'}" type="button" data-dialog-ok>${esc(options.confirmText ?? '确定')}</button>
        </div>
      </div>`;
    document.body.appendChild(overlay);

    const input = overlay.querySelector<HTMLInputElement>('.dialog-input');
    let settled = false;

    const finish = (value: string | null): void => {
      if (settled) return;
      settled = true;
      if (activeClose === finish) activeClose = null;
      document.removeEventListener('keydown', onKey, true);
      overlay.remove();
      previous?.focus?.();
      resolve(value);
    };
    const confirm = (): void => finish(input ? input.value.trim() : '');

    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        finish(null);
      }
    };
    document.addEventListener('keydown', onKey, true);

    overlay.querySelector('[data-dialog-cancel]')?.addEventListener('click', () => finish(null));
    overlay.querySelector('[data-dialog-ok]')?.addEventListener('click', confirm);
    // Enter submits from the input; buttons keep their native Enter action so
    // a focused 取消 stays a cancel instead of becoming a confirm.
    overlay.addEventListener('keydown', (e) => {
      if (e.key !== 'Enter' || e.shiftKey) return;
      if ((e.target as HTMLElement).closest('button')) return;
      e.preventDefault();
      confirm();
    });
    overlay.addEventListener('click', (e) => {
      if (e.target === overlay) finish(null);
    });

    activeClose = finish;
    input?.focus();
    input?.select();
  });
}

/** Ask for a single line of text. Resolves null when cancelled. */
export function promptDialog(options: PromptDialogOptions): Promise<string | null> {
  return openDialog({
    title: options.title,
    body: options.body,
    input: options,
    confirmText: options.confirmText,
    cancelText: options.cancelText,
  });
}

/** Ask yes/no. Resolves false when cancelled. */
export async function confirmDialog(options: ConfirmDialogOptions): Promise<boolean> {
  const answer = await openDialog({
    title: options.title,
    body: options.body,
    confirmText: options.confirmText,
    cancelText: options.cancelText,
    danger: options.danger,
  });
  return answer !== null;
}
