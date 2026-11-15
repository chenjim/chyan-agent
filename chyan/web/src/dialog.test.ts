// @vitest-environment happy-dom
import { beforeEach, describe, expect, it } from 'vitest';
import { confirmDialog, promptDialog } from './dialog';

const overlay = (): HTMLElement | null => document.querySelector('.dialog-overlay');

beforeEach(() => {
  document.body.innerHTML = '';
});

describe('promptDialog', () => {
  it('prefills the input and resolves the trimmed value on confirm', async () => {
    const pending = promptDialog({ title: '重命名', value: '旧标题' });
    const input = document.querySelector<HTMLInputElement>('.dialog-input')!;
    expect(input.value).toBe('旧标题');
    input.value = '  新标题  ';
    document.querySelector<HTMLButtonElement>('[data-dialog-ok]')!.click();
    await expect(pending).resolves.toBe('新标题');
    expect(overlay()).toBeNull();
  });

  it('submits on Enter inside the input', async () => {
    const pending = promptDialog({ title: '重命名' });
    const input = document.querySelector<HTMLInputElement>('.dialog-input')!;
    input.value = 'abc';
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await expect(pending).resolves.toBe('abc');
  });

  it('does not confirm when Enter is pressed on the cancel button', async () => {
    const pending = promptDialog({ title: '重命名', value: '旧标题' });
    const cancel = document.querySelector<HTMLButtonElement>('[data-dialog-cancel]')!;
    cancel.focus();
    cancel.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    expect(overlay()).not.toBeNull();
    cancel.click();
    await expect(pending).resolves.toBeNull();
  });

  it('applies a max length to the input', () => {
    void promptDialog({ title: '重命名', maxLength: 20 });
    expect(document.querySelector<HTMLInputElement>('.dialog-input')!.maxLength).toBe(20);
    document.querySelector<HTMLButtonElement>('[data-dialog-cancel]')!.click();
  });

  it('resolves null on Escape', async () => {
    const pending = promptDialog({ title: '重命名' });
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await expect(pending).resolves.toBeNull();
    expect(overlay()).toBeNull();
  });

  it('resolves null on the cancel button and backdrop click', async () => {
    const pending = promptDialog({ title: '重命名' });
    document.querySelector<HTMLButtonElement>('[data-dialog-cancel]')!.click();
    await expect(pending).resolves.toBeNull();

    const pending2 = promptDialog({ title: '重命名' });
    overlay()!.click();
    await expect(pending2).resolves.toBeNull();
  });

  it('keeps the dialog open when clicking inside it', async () => {
    const pending = promptDialog({ title: '重命名' });
    document.querySelector<HTMLElement>('.dialog')!.click();
    expect(overlay()).not.toBeNull();
    document.querySelector<HTMLButtonElement>('[data-dialog-ok]')!.click();
    await pending;
  });

  it('allows an empty value so the title can be reset', async () => {
    const pending = promptDialog({ title: '重命名', value: '旧标题' });
    document.querySelector<HTMLInputElement>('.dialog-input')!.value = '   ';
    document.querySelector<HTMLButtonElement>('[data-dialog-ok]')!.click();
    await expect(pending).resolves.toBe('');
  });

  it('escapes markup in the title and value', () => {
    void promptDialog({ title: '<b>x</b>', value: '<script>' });
    expect(document.querySelector('.dialog-title')!.innerHTML).toBe('&lt;b&gt;x&lt;/b&gt;');
    expect(document.querySelector<HTMLInputElement>('.dialog-input')!.value).toBe('<script>');
    // Close the dialog so it does not leak into the next test.
    document.querySelector<HTMLButtonElement>('[data-dialog-cancel]')!.click();
  });

  it('closes the previous dialog when a new one opens', async () => {
    const first = promptDialog({ title: '第一个' });
    const second = promptDialog({ title: '第二个' });
    await expect(first).resolves.toBeNull();
    expect(overlay()).not.toBeNull();
    expect(document.querySelector('.dialog-title')!.textContent).toBe('第二个');
    document.querySelector<HTMLButtonElement>('[data-dialog-cancel]')!.click();
    await second;
  });

  it('restores focus to the previously focused element', async () => {
    const button = document.createElement('button');
    document.body.appendChild(button);
    button.focus();
    const pending = promptDialog({ title: '重命名' });
    document.querySelector<HTMLButtonElement>('[data-dialog-cancel]')!.click();
    await pending;
    expect(document.activeElement).toBe(button);
  });
});

describe('confirmDialog', () => {
  it('resolves true on confirm and false on cancel', async () => {
    const accepted = confirmDialog({ title: '删除', body: '不可撤销' });
    document.querySelector<HTMLButtonElement>('[data-dialog-ok]')!.click();
    await expect(accepted).resolves.toBe(true);

    const rejected = confirmDialog({ title: '删除' });
    document.querySelector<HTMLButtonElement>('[data-dialog-cancel]')!.click();
    await expect(rejected).resolves.toBe(false);
  });

  it('renders a destructive confirm button when requested', () => {
    void confirmDialog({ title: '删除', danger: true });
    expect(document.querySelector('[data-dialog-ok]')!.className).toContain('danger');
    document.querySelector<HTMLButtonElement>('[data-dialog-cancel]')!.click();
  });

  it('has no text input', () => {
    void confirmDialog({ title: '删除' });
    expect(document.querySelector('.dialog-input')).toBeNull();
    document.querySelector<HTMLButtonElement>('[data-dialog-cancel]')!.click();
  });
});
