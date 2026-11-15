// Approval panel: pending tickets with diff preview and approve/deny buttons.

import type { ApiClient } from '../api';
import { renderDiffHtml } from '../diff';
import { esc, fmtTime } from '../format';
import { chip, emptyState, errorState, skeleton, type View } from '../render';
import type { ApprovalTicket } from '../types';

const VERDICT_LABEL: Record<string, string> = {
  low: 'AI 判定：低风险',
  high: 'AI 判定：高风险',
  uncertain: 'AI 判定：不确定',
};

export function approvalsView(api: ApiClient): View {
  let disposed = false;
  const html = `
    <section class="page">
      <h2 class="section-title">待审批</h2>
      <div id="approval-list">${skeleton(3)}</div>
    </section>`;

  const reload = async (root: HTMLElement) => {
    const box = root.querySelector<HTMLElement>('#approval-list');
    if (!box) return;
    try {
      const tickets = await api.listApprovals();
      if (disposed) return;
      box.innerHTML = tickets.length
        ? tickets.map((t) => renderTicket(t)).join('')
        : emptyState('没有待审批操作');
    } catch (err) {
      if (!disposed) box.innerHTML = errorState(err);
    }
  };

  return {
    html,
    mounted(root) {
      void reload(root);
      // Delegated decision handler (cards are re-rendered after each click).
      root.addEventListener('click', (e) => {
        const btn = (e.target as HTMLElement).closest<HTMLButtonElement>('button[data-decide]');
        if (!btn) return;
        const id = btn.dataset.id ?? '';
        const decision = btn.dataset.decide ?? '';
        btn.disabled = true;
        void (async () => {
          try {
            await api.decideApproval(id, { decision });
          } catch {
            // network error: re-enable so the reviewer can retry
            btn.disabled = false;
          }
          await reload(root);
        })();
      });
    },
    refresh(root) {
      void reload(root);
    },
    cleanup() {
      disposed = true;
    },
  };
}

/** Render one approval ticket card (pure; exported for tests). */
export function renderTicket(t: ApprovalTicket): string {
  const verdict = t.verdict ? chip(VERDICT_LABEL[t.verdict] ?? t.verdict, `v-${t.verdict}`) : '';
  return `
    <div class="card approval-card" id="ap-${esc(t.id)}">
      <div class="row">
        ${chip(t.action, 'action')}
        <span class="mono dim">${esc(t.id.slice(0, 8))}</span>
        <span class="dim time">${fmtTime(t.created_at)}</span>
      </div>
      <div class="resource mono">${esc(t.resource)}</div>
      ${verdict}
      ${renderDiffHtml(t.diff ?? '', esc)}
      <div class="row btn-row">
        <button class="btn primary big" data-decide="approve" data-id="${esc(t.id)}">批准</button>
        <button class="btn danger big" data-decide="deny" data-id="${esc(t.id)}">拒绝</button>
      </div>
    </div>`;
}
