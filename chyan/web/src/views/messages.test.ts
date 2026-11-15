// @vitest-environment happy-dom
import { describe, expect, it } from 'vitest';
import type { Message } from '../types';
import { renderMessage, renderSessionTitle } from './messages';

function message(over: Partial<Message> = {}): Message {
  return {
    id: 'm1',
    role: 'assistant',
    text: 'hello',
    created_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

describe('renderSessionTitle', () => {
  it('falls back to the short id', () => {
    expect(renderSessionTitle({ id: 'sess1234', created_at: '', updated_at: '' })).toBe('sess1234');
    expect(
      renderSessionTitle({ id: 'sess1234', title: '标题', created_at: '', updated_at: '' }),
    ).toBe('标题');
  });
});

describe('renderMessage', () => {
  it('renders user turns as bubbles without a role label', () => {
    const html = renderMessage(message({ role: 'user', text: '<hi>' }));
    expect(html).toContain('msg-user');
    expect(html).toContain('&lt;hi&gt;');
    expect(html).not.toContain('用户');
  });

  it('keeps raw tool output behind a collapsible summary', () => {
    const html = renderMessage(message({ role: 'tool', text: 'x'.repeat(50) }));
    expect(html).toContain('tool-output');
    expect(html).toContain('<summary>工具输出</summary>');
  });

  it('labels the tool summary with the call that produced it', () => {
    const html = renderMessage(
      message({ role: 'tool', text: 'ok', tool: 'bash · sqlite3 <db>' }),
    );
    expect(html).toContain('<summary>bash · sqlite3 &lt;db&gt;</summary>');
  });

  it('passes unknown roles through and lists tool calls', () => {
    const html = renderMessage(message({ role: 'critic', tool_calls: ['bash', 'read'] }));
    expect(html).toContain('critic');
    expect(html).toContain('tool-row');
    expect(html).toContain('bash');
  });

  it('drops empty assistant turns with no tool calls', () => {
    expect(renderMessage(message({ text: '  ' }))).toBe('');
    expect(renderMessage(message({ text: '', tool_calls: ['bash'] }))).toContain('tool-row');
  });

  it('adds a quiet action row under assistant text only', () => {
    const html = renderMessage(message());
    expect(html).toContain('msg-actions');
    expect((html.match(/data-msg-action=/g) ?? []).length).toBe(5);
    expect(renderMessage(message({ role: 'user' }))).not.toContain('msg-actions');
  });

  it('renders assistant markdown and strips unsafe html', () => {
    const html = renderMessage(message({ text: '## Hi\n\n- **bold**\n- `code`' }));
    expect(html).toContain('msg-text md');
    expect(html).toContain('<strong>bold</strong>');
    expect(html).toContain('<code>code</code>');
    const xss = renderMessage(message({ text: '<img src=x onerror="alert(1)">' }));
    expect(xss).not.toContain('onerror');
  });

  it('renders thumbnails for images attached to a user turn', () => {
    const html = renderMessage(message({ role: 'user', images: ['/abs/a.png'] }));
    expect(html).toContain('msg-images');
    expect(html).toContain('data-msg-image="/abs/a.png"');
  });

  it('renders code fences, tables and links in assistant markdown', () => {
    const html = renderMessage(
      message({ text: '```py\nprint(1)\n```\n\n| a | b |\n| --- | --- |\n| 1 | 2 |\n\n[link](https://example.com)' }),
    );
    expect(html).toContain('<code class="language-py">');
    expect(html).toContain('<table>');
    expect(html).toContain('<a href="https://example.com">link</a>');
  });

  it('strips scripts and javascript urls from model output', () => {
    const script = renderMessage(message({ text: '<script>alert(1)</script>' }));
    expect(script).not.toContain('<script');
    const jsUrl = renderMessage(message({ text: '[x](javascript:alert(1))' }));
    const box = document.createElement('div');
    box.innerHTML = jsUrl;
    expect(box.querySelector('a')?.getAttribute('href') ?? '').not.toMatch(/javascript:/i);
  });

  it('leaves user text as literal text instead of markdown', () => {
    const html = renderMessage(message({ role: 'user', text: '**not bold**' }));
    expect(html).toContain('**not bold**');
    expect(html).not.toContain('<strong>');
  });
});
