// REST client for the daemon API contract (chyan/internal/api/types.go).
// Base path /api/v1; auth: Authorization: Bearer <token>.

import type {
  ApprovalDecision,
  ApprovalTicket,
  ContextUsage,
  CreateTaskRequest,
  CronJob,
  CronJobRequest,
  DreamReport,
  FileContent,
  FileNode,
  MemoryEntry,
  Message,
  SearchHit,
  Session,
  SessionSummary,
  Status,
  Task,
  TraceResponse,
  WorkspaceInfo,
} from './types';

export class ApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

export interface ApiConfig {
  base?: string; // default '/api/v1'
  token: string;
  fetchImpl?: typeof fetch;
}

export class ApiClient {
  private readonly base: string;
  private readonly token: string;
  private readonly fetchImpl: typeof fetch;

  constructor(cfg: ApiConfig) {
    this.base = cfg.base ?? '/api/v1';
    this.token = cfg.token;
    this.fetchImpl = cfg.fetchImpl ?? fetch.bind(globalThis);
  }

  // ---- tasks ----
  listTasks(): Promise<Task[]> {
    return this.get<Task[]>('/tasks');
  }

  getTask(id: string): Promise<Task> {
    return this.get<Task>(`/tasks/${encodeURIComponent(id)}`);
  }

  createTask(req: CreateTaskRequest): Promise<Task> {
    return this.post<Task>('/tasks', req);
  }

  // cancelTask interrupts a running task (idempotent: a finished task comes
  // back with its terminal status).
  cancelTask(id: string): Promise<Task> {
    return this.post<Task>(`/tasks/${encodeURIComponent(id)}/cancel`, {});
  }

  // ---- approvals ----
  listApprovals(): Promise<ApprovalTicket[]> {
    return this.get<ApprovalTicket[]>('/approvals');
  }

  decideApproval(id: string, decision: ApprovalDecision): Promise<ApprovalTicket> {
    return this.post<ApprovalTicket>(`/approvals/${encodeURIComponent(id)}`, decision);
  }

  // ---- sessions ----
  listSessions(): Promise<SessionSummary[]> {
    return this.get<SessionSummary[]>('/sessions');
  }

  getSession(id: string): Promise<Session> {
    return this.get<Session>(`/sessions/${encodeURIComponent(id)}`);
  }

  /** Live window usage; the header shows this instead of cumulative tokens. */
  sessionContext(id: string): Promise<ContextUsage> {
    return this.get<ContextUsage>(`/sessions/${encodeURIComponent(id)}/context`);
  }

  sessionMessages(id: string): Promise<Message[]> {
    return this.get<Message[]>(`/sessions/${encodeURIComponent(id)}/messages`);
  }

  renameSession(id: string, title: string): Promise<SessionSummary> {
    return this.request<SessionSummary>(`/sessions/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ title }),
    });
  }

  async deleteSession(id: string): Promise<void> {
    const res = await this.fetchImpl(this.buildUrl(`/sessions/${encodeURIComponent(id)}`), {
      method: 'DELETE',
      headers: { Authorization: `Bearer ${this.token}` },
    });
    await this.checkOk(res);
  }

  // search looks inside message content, not just titles.
  search(q: string): Promise<SearchHit[]> {
    return this.get<SearchHit[]>('/search', { q });
  }

  /** Per-turn spans and attribution for one session (FR-13 trace viewer). */
  traces(sessionId: string, limit = 0): Promise<TraceResponse> {
    return this.get<TraceResponse>('/traces', {
      session_id: sessionId,
      limit: limit > 0 ? String(limit) : '',
    });
  }

  async exportSession(id: string): Promise<Blob> {
    const res = await this.fetchImpl(this.buildUrl(`/sessions/${encodeURIComponent(id)}/export`), {
      method: 'GET',
      headers: { Authorization: `Bearer ${this.token}` },
    });
    await this.checkOk(res);
    return res.blob();
  }

  // ---- workspace ----
  getWorkspace(): Promise<WorkspaceInfo> {
    return this.get<WorkspaceInfo>('/workspace');
  }

  // ---- files (read-only) ----
  listFiles(path = '', workspace = '', source = ''): Promise<FileNode[]> {
    return this.get<FileNode[]>('/files', { path, workspace, source });
  }

  readFile(path: string, workspace = '', source = ''): Promise<FileContent> {
    return this.get<FileContent>('/files/content', { path, workspace, source });
  }

  async downloadFile(path: string, workspace = '', source = ''): Promise<Blob> {
    const res = await this.fetchImpl(this.buildUrl('/files/download', { path, workspace, source }), {
      method: 'GET',
      headers: { Authorization: `Bearer ${this.token}` },
    });
    await this.checkOk(res);
    return res.blob();
  }

  // ---- status ----
  status(): Promise<Status> {
    return this.get<Status>('/status');
  }

  // ---- cron (scheduled automation) ----
  listCron(): Promise<CronJob[]> {
    return this.get<CronJob[]>('/cron');
  }

  createCron(req: CronJobRequest): Promise<CronJob> {
    return this.post<CronJob>('/cron', req);
  }

  updateCron(id: string, req: CronJobRequest): Promise<CronJob> {
    return this.request<CronJob>(`/cron/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(req),
    });
  }

  async deleteCron(id: string): Promise<void> {
    const res = await this.fetchImpl(this.buildUrl(`/cron/${encodeURIComponent(id)}`), {
      method: 'DELETE',
      headers: { Authorization: `Bearer ${this.token}` },
    });
    await this.checkOk(res);
  }

  runCron(id: string): Promise<{ started: boolean }> {
    return this.post<{ started: boolean }>(`/cron/${encodeURIComponent(id)}/run`, {});
  }

  // ---- long-term memory ----
  listMemory(params: { q?: string; domain?: string; kind?: string; project?: string } = {}): Promise<MemoryEntry[]> {
    return this.get<MemoryEntry[]>('/memory', {
      q: params.q ?? '',
      domain: params.domain ?? '',
      kind: params.kind ?? '',
      project: params.project ?? '',
    });
  }

  supersedeMemory(id: string, project = ''): Promise<MemoryEntry> {
    return this.post<MemoryEntry>(
      `/memory/${encodeURIComponent(id)}/supersede`,
      {},
      project ? { project } : undefined,
    );
  }

  getDream(): Promise<DreamReport> {
    return this.get<DreamReport>('/memory/dream');
  }

  runDream(): Promise<DreamReport> {
    return this.post<DreamReport>('/memory/dream', {});
  }

  private get<T>(path: string, query?: Record<string, string>): Promise<T> {
    return this.request<T>(path, { method: 'GET' }, query);
  }

  private post<T>(path: string, body: unknown, query?: Record<string, string>): Promise<T> {
    return this.request<T>(
      path,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      },
      query,
    );
  }

  private async request<T>(path: string, init: RequestInit, query?: Record<string, string>): Promise<T> {
    const res = await this.fetchImpl(this.buildUrl(path, query), {
      ...init,
      headers: { Authorization: `Bearer ${this.token}`, ...(init.headers as Record<string, string>) },
    });
    await this.checkOk(res);
    return (await res.json()) as T;
  }

  private buildUrl(path: string, query?: Record<string, string>): string {
    let url = this.base + path;
    if (query) {
      const qs = new URLSearchParams();
      for (const [k, v] of Object.entries(query)) if (v !== '') qs.set(k, v);
      const s = qs.toString();
      if (s) url += `?${s}`;
    }
    return url;
  }

  private async checkOk(res: Response): Promise<void> {
    if (res.ok) return;
    let msg = `${res.status}`;
    try {
      const data = (await res.json()) as { error?: string };
      if (data.error) msg = data.error;
    } catch {
      // non-JSON error body; keep status text
    }
    throw new ApiError(res.status, msg);
  }
}
