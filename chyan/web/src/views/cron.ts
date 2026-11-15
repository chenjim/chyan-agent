// Cron (automation) view: list scheduled jobs with their last status and
// linked session, create/edit/toggle/delete them and run one immediately
// (FR-12 W1).

import type { ApiClient } from '../api';
import { confirmDialog } from '../dialog';
import { esc, fmtTime } from '../format';
import { emptyState, errorState, skeleton, type View } from '../render';
import type { CronJob, CronJobRequest } from '../types';

export interface CronViewOptions {
  /** Open the transcript of the session a job last ran. */
  onOpenSession?: (id: string) => void;
}

export function cronView(api: ApiClient, options: CronViewOptions = {}): View {
  let disposed = false;
  let editing: string | null = null;
  let rootEl: HTMLElement | null = null;
  const html = `
    <section class="page">
      <h2 class="section-title">自动化</h2>
      <form id="cron-form" class="card cron-form">
        <input id="cron-name" class="input" placeholder="任务名称" />
        <input id="cron-schedule" class="input mono" placeholder="定时表达式（*/30 * * * * 或 @every 1h）" />
        <textarea id="cron-prompt" class="input" rows="3" placeholder="到点后要执行的内容"></textarea>
        <input id="cron-channel" class="input" placeholder="投递渠道（可选，如 feishu:oc_x）" />
        <input id="cron-workdir" class="input mono" placeholder="工作区（可选，留空用默认）" list="cron-workdirs" />
        <datalist id="cron-workdirs"></datalist>
        <label class="row"><input id="cron-enabled" type="checkbox" checked /> 启用</label>
        <div class="row">
          <button class="btn primary" type="submit" id="cron-save">保存</button>
          <button class="btn ghost" type="button" id="cron-cancel" hidden>取消编辑</button>
        </div>
        <div id="cron-form-error" class="err-text"></div>
      </form>
      <div id="cron-body">${skeleton(4)}</div>
    </section>`;

  const listBody = (root: HTMLElement) => root.querySelector<HTMLElement>('#cron-body');
  const formError = (root: HTMLElement) => root.querySelector<HTMLElement>('#cron-form-error');

  const fillForm = (root: HTMLElement, job: CronJob | null) => {
    editing = job?.id ?? null;
    const set = (id: string, value: string) => {
      const el = root.querySelector<HTMLInputElement>(`#${id}`);
      if (el) el.value = value;
    };
    set('cron-name', job?.name ?? '');
    set('cron-schedule', job?.schedule ?? '');
    set('cron-prompt', job?.prompt ?? '');
    set('cron-channel', job?.channel ?? '');
    set('cron-workdir', job?.workdir ?? '');
    const enabled = root.querySelector<HTMLInputElement>('#cron-enabled');
    if (enabled) enabled.checked = job?.enabled ?? true;
    const save = root.querySelector<HTMLElement>('#cron-save');
    if (save) save.textContent = editing ? '保存修改' : '保存';
    const cancel = root.querySelector<HTMLElement>('#cron-cancel');
    if (cancel) cancel.hidden = !editing;
    const err = formError(root);
    if (err) err.textContent = '';
  };

  const readForm = (root: HTMLElement): CronJobRequest => ({
    name: root.querySelector<HTMLInputElement>('#cron-name')?.value.trim() ?? '',
    schedule: root.querySelector<HTMLInputElement>('#cron-schedule')?.value.trim() ?? '',
    prompt: root.querySelector<HTMLTextAreaElement>('#cron-prompt')?.value ?? '',
    channel: root.querySelector<HTMLInputElement>('#cron-channel')?.value.trim() ?? '',
    workdir: root.querySelector<HTMLInputElement>('#cron-workdir')?.value.trim() ?? '',
    enabled: root.querySelector<HTMLInputElement>('#cron-enabled')?.checked ?? true,
  });

  const renderList = (root: HTMLElement, jobs: CronJob[]) => {
    const box = listBody(root);
    if (!box) return;
    box.innerHTML = jobs.length
      ? `<div class="cron-list">${jobs.map(renderCronJob).join('')}</div>`
      : emptyState('还没有定时任务');
  };

  const load = async (root: HTMLElement) => {
    try {
      const jobs = await api.listCron();
      if (disposed) return;
      renderList(root, jobs);
    } catch (err) {
      if (!disposed) {
        const box = listBody(root);
        if (box) box.innerHTML = errorState(err);
      }
    }
  };

  const runAction = async (root: HTMLElement, fn: () => Promise<unknown>) => {
    try {
      await fn();
      await load(root);
    } catch (e) {
      const box = formError(root);
      if (box) box.textContent = e instanceof Error ? e.message : String(e);
    }
  };

  const toggleEnabled = (root: HTMLElement, job: CronJob) =>
    runAction(root, () => api.updateCron(job.id, { ...toRequest(job), enabled: !job.enabled }));

  return {
    html,
    mounted(root) {
      rootEl = root;
      root.querySelector('#cron-form')?.addEventListener('submit', (e) => {
        e.preventDefault();
        const req = readForm(root);
        const err = formError(root);
        if (err) err.textContent = '';
        if (!req.name || (!req.schedule && !req.prompt)) {
          if (err) err.textContent = '名称和定时/内容不能为空';
          return;
        }
        void (editing
          ? api.updateCron(editing, req).then(() => {
              fillForm(root, null);
              return load(root);
            })
          : api.createCron(req).then(() => {
              fillForm(root, null);
              return load(root);
            })
        ).catch((e2: unknown) => {
          if (err) err.textContent = e2 instanceof Error ? e2.message : String(e2);
        });
      });
      root.querySelector('#cron-cancel')?.addEventListener('click', () => fillForm(root, null));

      root.addEventListener('click', (e) => {
        const el = (e.target as HTMLElement).closest<HTMLElement>('[data-cron-id]');
        if (!el) return;
        // Buttons carry only the id; the card carries the job snapshot.
        const card = el.closest<HTMLElement>('[data-cron-job]') ?? el;
        const job = parseJob(card.dataset.cronJob);
        if (!job) return;
        const id = el.dataset.cronId ?? '';
        if (el.dataset.cronRun !== undefined) void runAction(root, () => api.runCron(id));
        else if (el.dataset.cronToggle !== undefined) void toggleEnabled(root, job);
        else if (el.dataset.cronEdit !== undefined) fillForm(root, job);
        else if (el.dataset.cronDelete !== undefined) {
          void (async () => {
            const ok = await confirmDialog({
              title: '删除定时任务',
              body: `确定删除「${job.name}」？`,
              confirmText: '删除',
              danger: true,
            });
            if (ok) await runAction(root, () => api.deleteCron(id));
          })();
        } else if (el.dataset.cronSession !== undefined) {
          const sid = el.dataset.cronSession;
          if (sid) options.onOpenSession?.(sid);
        }
      });

      void api.getWorkspace().then((w) => {
        if (disposed) return;
        const dl = root.querySelector<HTMLElement>('#cron-workdirs');
        if (dl) dl.innerHTML = w.recent.map((p) => `<option value="${esc(p)}"></option>`).join('');
      }).catch(() => undefined);
      void load(root);
    },
    refresh(root) {
      void load(root);
    },
    onEvent(ev) {
      if (ev.name === 'cron.run' && rootEl) void load(rootEl);
    },
    cleanup() {
      disposed = true;
      rootEl = null;
    },
  };
}

// toRequest converts a stored job back to its editable definition.
function toRequest(job: CronJob): CronJobRequest {
  return {
    name: job.name,
    schedule: job.schedule,
    prompt: job.prompt,
    enabled: job.enabled,
    channel: job.channel,
    workdir: job.workdir,
    script: job.script,
    no_agent: job.no_agent,
    after: job.after,
    context_from: job.context_from,
  };
}

// The job travels with the action button (data attribute JSON) so a click
// does not need a second lookup in view state.
export function renderCronJob(job: CronJob): string {
  const status = job.last_status === 'ok'
    ? '<span class="chip ok">上次成功</span>'
    : job.last_status === 'error'
      ? '<span class="chip err">上次失败</span>'
      : '<span class="chip dim">未运行</span>';
  const enabled = job.enabled
    ? '<span class="chip">已启用</span>'
    : '<span class="chip dim">已停用</span>';
  const session = job.last_session_id
    ? `<button class="btn small ghost" data-cron-id="${esc(job.id)}" data-cron-session="${esc(job.last_session_id)}">查看会话</button>`
    : '';
  return `
    <div class="card cron-card" data-cron-id="${esc(job.id)}" data-cron-job="${esc(JSON.stringify(job))}">
      <div class="row">
        <span class="cron-name">${esc(job.name)}</span>
        ${enabled}${status}
      </div>
      <div class="dim mono">${esc(job.schedule || '事件触发')}</div>
      <div class="cron-prompt dim">${esc(job.prompt)}</div>
      ${job.last_run_at ? `<div class="dim time">上次运行 ${fmtTime(job.last_run_at)}</div>` : ''}
      ${job.last_error ? `<div class="err-text">${esc(job.last_error)}</div>` : ''}
      <div class="row">
        <button class="btn small primary" data-cron-id="${esc(job.id)}" data-cron-run="1">运行</button>
        <button class="btn small" data-cron-id="${esc(job.id)}" data-cron-toggle="1">${job.enabled ? '停用' : '启用'}</button>
        <button class="btn small" data-cron-id="${esc(job.id)}" data-cron-edit="1">编辑</button>
        <button class="btn small ghost" data-cron-id="${esc(job.id)}" data-cron-delete="1">删除</button>
        ${session}
      </div>
    </div>`;
}

function parseJob(raw: string | undefined): CronJob | null {
  if (!raw) return null;
  try {
    return JSON.parse(raw) as CronJob;
  } catch {
    return null;
  }
}
