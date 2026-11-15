// Shared transcript rendering for the merged conversation hub.

import DOMPurifyFactory from 'dompurify';
import { marked } from 'marked';

import { esc, fmtTime } from '../format';
import { chip } from '../render';
import type { Message, SessionSummary } from '../types';

const ROLE_LABEL: Record<string, string> = {
  system: '系统',
};

type Sanitizer = { sanitize: (html: string, options?: Record<string, unknown>) => string };

// The ESM build exports the sanitizer in browsers but a factory when it was
// evaluated without a document (tests/SSR). Accept both shapes.
const factory = DOMPurifyFactory as unknown as Sanitizer & ((win?: Window) => Sanitizer);
const purify: Sanitizer =
  typeof factory.sanitize === 'function'
    ? factory
    : factory(typeof window === 'undefined' ? undefined : window);

/** Assistant content is model output: render Markdown, then strip unsafe HTML. */
export function renderMarkdown(text: string): string {
  const html = marked.parse(text, { async: false });
  if (typeof purify.sanitize !== 'function') return esc(html);
  let safe = purify.sanitize(html, { USE_PROFILES: { html: true } });
  // Defence in depth: DOMPurify already drops script URLs in browsers, but a
  // minimal DOM (tests) may keep the attribute. Strip the protocol here too.
  const template = document.createElement('template');
  template.innerHTML = safe;
  template.content.querySelectorAll('a[href], img[src]').forEach((el) => {
    const attr = el.hasAttribute('href') ? 'href' : 'src';
    if (/^(javascript|vbscript):/i.test((el.getAttribute(attr) ?? '').trim())) {
      el.removeAttribute(attr);
    }
  });
  safe = template.innerHTML;
  return safe;
}

const ACTION_ICONS = {
  copy: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="9" y="9" width="10" height="10" rx="2"/><path d="M5 15V7a2 2 0 012-2h8"/></svg>',
  like: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 11v8H4.5a1 1 0 01-1-1V12a1 1 0 011-1z"/><path d="M7 11l3.8-6.6a1.8 1.8 0 013.2 1.6V10h4.6a2 2 0 012 2.4l-1.2 5.6a2 2 0 01-2 1.6H7"/></svg>',
  dislike: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 13V5h-1.5a1 1 0 00-1 1v6a1 1 0 001 1z"/><path d="M7 13l3.8 6.6a1.8 1.8 0 003.2-1.6V14h4.6a2 2 0 002-2.4l-1.2-5.6a2 2 0 00-2-1.6H7"/></svg>',
  speak: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 10v4h3l4 3V7l-4 3z"/><path d="M15 9a4 4 0 010 6"/><path d="M17.5 6.5a8 8 0 010 11"/></svg>',
  share: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M14 5h5v5"/><path d="M19 5l-7 7"/><path d="M12 5H7a2 2 0 00-2 2v10a2 2 0 002 2h10a2 2 0 002-2v-5"/></svg>',
} as const;

/** Quiet action row under an assistant reply (copy / feedback / read / share). */
function assistantActions(): string {
  const button = (action: keyof typeof ACTION_ICONS, label: string) => `
    <button class="msg-action" type="button" data-msg-action="${action}"
            aria-label="${label}" title="${label}">${ACTION_ICONS[action]}</button>`;
  return `
    <div class="msg-actions">
      ${button('copy', '复制')}
      ${button('like', '有帮助')}
      ${button('dislike', '没帮助')}
      ${button('speak', '朗读')}
      ${button('share', '分享')}
    </div>`;
}

export function renderSessionTitle(s: SessionSummary): string {
  return s.title || s.id.slice(0, 8);
}

export function renderMessage(m: Message): string {
  // Assistant turns that carried only a tool call often persist an empty
  // text part; rendering that would leave a blank bubble in the transcript.
  const text = (m.text ?? '').trim();
  const tools = (m.tool_calls ?? []).map((t) => chip(t, 'mono dim')).join('');
  if (!text && !tools) return '';

  // Chat transcript: user turns are bubbles on the right, assistant turns are
  // open text on the left, and neither shows a role label or timestamp.
  if (m.role === 'user' || m.role === 'assistant') {
    const isAssistant = m.role === 'assistant';
    const images =
      !isAssistant && m.images?.length
        ? `<div class="msg-images">${m.images
            .map((p) => `<img class="msg-image" data-msg-image="${esc(p)}" alt="会话图片" />`)
            .join('')}</div>`
        : '';
    const body = text
      ? isAssistant
        ? `<div class="msg-text md">${renderMarkdown(m.text)}</div>`
        : `<div class="msg-text">${esc(m.text)}</div>`
      : '';
    const actions = isAssistant && text ? assistantActions() : '';
    return `
      <div class="msg msg-${esc(m.role)}">
        ${images}
        ${body}
        ${tools ? `<div class="row tool-row">${tools}</div>` : ''}
        ${actions}
      </div>`;
  }

  // Raw tool output is long and noisy; keep it one click away. The label
  // names the call that produced it ("bash · ls") when the API provides one.
  if (m.role === 'tool') {
    if (!text) return `<div class="row tool-row">${tools}</div>`;
    const label = (m.tool ?? '').trim() || '工具输出';
    return `
      <details class="tool-output msg-tool">
        <summary>${esc(label)}</summary>
        <div class="msg-text">${esc(m.text)}</div>
      </details>`;
  }

  const role = ROLE_LABEL[m.role] ?? m.role;
  return `
    <div class="msg msg-${esc(m.role)}">
      <div class="row">
        ${chip(role, `role-${esc(m.role)}`)}
        <span class="dim time">${fmtTime(m.created_at)}</span>
      </div>
      ${text ? `<div class="msg-text">${esc(m.text)}</div>` : ''}
      ${tools ? `<div class="row tool-row">${tools}</div>` : ''}
    </div>`;
}
