// @vitest-environment happy-dom
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api';
import type { ApprovalTicket } from '../types';
import { approvalsView, renderTicket } from './approvals';

function ticket(over: Partial<ApprovalTicket> = {}): ApprovalTicket {
  return {
    id: 'ap123456',
    action: 'bash',
    resource: 'rm -rf build',
    created_at: '2024-01-01T00:00:00Z',
    ...over,
  };
}

describe('renderTicket', () => {
  it('renders the action, resource and decision buttons', () => {
    const html = renderTicket(ticket());
    expect(html).toContain('ap123456');
    expect(html).toContain('bash');
    expect(html).toContain('rm -rf build');
    expect(html).toContain('data-decide="approve"');
    expect(html).toContain('data-decide="deny"');
  });

  it('maps reviewer verdicts and escapes unknown ones', () => {
    expect(renderTicket(ticket({ verdict: 'low' }))).toContain('AI 判定：低风险');
    expect(renderTicket(ticket({ verdict: 'high' }))).toContain('AI 判定：高风险');
    expect(renderTicket(ticket({ verdict: 'uncertain' }))).toContain('AI 判定：不确定');
    // Unknown verdicts are shown verbatim (no AI label prefix).
    expect(renderTicket(ticket({ verdict: 'custom' }))).toContain('>custom</span>');
  });

  it('escapes the resource and omits the verdict chip when absent', () => {
    const html = renderTicket(ticket({ resource: '<script>' }));
    expect(html).toContain('&lt;script&gt;');
    expect(html).not.toContain('AI 判定');
  });

  it('includes the diff block when provided', () => {
    const html = renderTicket(ticket({ diff: '--- a\n+++ b\n+added' }));
    expect(html).toContain('added');
  });
});

function fakeApi(over: Partial<ApiClient> = {}): ApiClient {
  return {
    listApprovals: vi.fn(async () => [ticket()]),
    decideApproval: vi.fn(async () => undefined),
    ...over,
  } as unknown as ApiClient;
}

describe('approvalsView', () => {
  it('shows an empty state without tickets', async () => {
    const api = fakeApi({ listApprovals: vi.fn(async () => []) });
    const root = document.createElement('div');
    const view = approvalsView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => {
      expect(root.querySelector('#approval-list')?.textContent).toContain('没有待审批操作');
    });
  });

  it('decides a ticket and reloads the list', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    const view = approvalsView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => {
      expect(root.querySelector('[data-decide="approve"]')).not.toBeNull();
    });
    root.querySelector<HTMLButtonElement>('[data-decide="approve"]')!.click();
    await vi.waitFor(() => {
      expect(api.decideApproval).toHaveBeenCalledWith('ap123456', { decision: 'approve' });
    });
  });

  it('re-enables the button when deciding fails', async () => {
    const api = fakeApi({ decideApproval: vi.fn(async () => Promise.reject(new Error('offline'))) });
    const root = document.createElement('div');
    const view = approvalsView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => {
      expect(root.querySelector('[data-decide="deny"]')).not.toBeNull();
    });
    const btn = root.querySelector<HTMLButtonElement>('[data-decide="deny"]')!;
    btn.click();
    await vi.waitFor(() => {
      const current = root.querySelector<HTMLButtonElement>('[data-decide="deny"]');
      expect(current?.disabled).toBe(false);
    });
  });

  it('refresh re-fetches and cleanup disposes the view', async () => {
    const api = fakeApi();
    const root = document.createElement('div');
    const view = approvalsView(api);
    root.innerHTML = view.html;
    view.mounted?.(root);
    await vi.waitFor(() => {
      expect(root.querySelector('[data-decide="approve"]')).not.toBeNull();
    });
    const calls = (api.listApprovals as ReturnType<typeof vi.fn>).mock.calls.length;
    view.refresh?.(root);
    await vi.waitFor(() => {
      expect((api.listApprovals as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThan(calls);
    });
    view.cleanup?.();
  });
});
