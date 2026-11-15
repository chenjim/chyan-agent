// Mirror of chyan/internal/api/types.go — REST/SSE contract (client side).

export interface Task {
  id: string;
  session_id: string;
  status: string; // pending|running|done|failed|interrupted
  reply?: string;
  err?: string;
  created_at: string;
  updated_at: string;
}

export interface CreateTaskRequest {
  prompt: string;
  session_id?: string;
  agent?: string;
  /** Per-task model override ("" = config/agent default). */
  model?: string;
  /** Workspace root on the daemon machine ("" = daemon default). */
  work_dir?: string;
  /** Data URLs (data:image/png;base64,...), max 4, each <= 5MB. */
  images?: string[];
  /** Replay the session's last user turn instead of appending a new one. */
  retry?: boolean;
}

export interface WorkspaceInfo {
  path: string;
  recent: string[];
  /** Absolute root for spilled tool output (artifact previews). */
  artifacts?: string;
}

export interface SessionSummary {
  id: string;
  title?: string;
  model?: string;
  status?: string; // active|interrupted
  /** Cumulative usage for this session (sub-agent cost rolls up). */
  cost_usd?: number;
  tokens_in?: number;
  tokens_out?: number;
  cache_read_tokens?: number;
  reasoning_tokens?: number;
  created_at: string;
  updated_at: string;
}

/** One message-level hit from the conversation search dialog. */
export interface SearchHit {
  session_id: string;
  title: string;
  model?: string;
  role: string;
  snippet: string;
  created_at: string;
  updated_at: string;
}

export interface Message {
  id: string;
  role: string;
  text: string;
  tool_calls?: string[];
  /** Label for a role=tool message, e.g. "bash · ls" (empty for old rows). */
  tool?: string;
  /** Artifact paths for image parts (absolute, inside the artifact root). */
  images?: string[];
  created_at: string;
}

export interface Session extends SessionSummary {
  messages: Message[];
  /** Live window usage, bundled with the transcript to avoid a second read. */
  context?: ContextUsage;
}

export interface ApprovalTicket {
  id: string;
  session_id?: string;
  action: string;
  resource: string;
  diff?: string;
  verdict?: string; // ai reviewer hint: low|high|uncertain
  created_at: string;
}

export interface ApprovalDecision {
  decision: string; // approve|deny
  reason?: string;
}

export interface FileNode {
  name: string;
  path: string;
  is_dir: boolean;
  size?: number;
}

export interface FileContent {
  path: string;
  content: string;
  truncated?: boolean;
}

export interface Status {
  version: string;
  sessions: number;
  pending_approvals: number;
  running_tasks?: number;
  review_mode: string;
}

/**
 * Live window usage of one session. Unlike the cumulative token counters this
 * resets with compaction, so it answers "will the next turn still fit".
 */
export interface ContextUsage {
  context_limit: number;
  fixed_cost: number;
  history_tokens: number;
  history_bytes?: number;
  used: number;
  soft_limit: number;
  hard_limit: number;
}

export interface CronJob {
  id: string;
  name: string;
  schedule: string;
  prompt: string;
  channel?: string;
  enabled: boolean;
  workdir?: string;
  script?: string;
  no_agent?: boolean;
  after?: string[];
  context_from?: string[];
  last_run_at?: string;
  last_status?: string;
  last_error?: string;
  last_session_id?: string;
}

export interface CronJobRequest {
  name: string;
  schedule: string;
  prompt: string;
  enabled: boolean;
  channel?: string;
  workdir?: string;
  script?: string;
  no_agent?: boolean;
  after?: string[];
  context_from?: string[];
}

export interface MemoryEntry {
  id: string;
  kind: string;
  domain: string;
  project_path?: string;
  name: string;
  keywords?: string[];
  triggers?: string[];
  content: string;
  status: string;
  path: string;
  created_at: string;
}

export interface DreamChange {
  ID: string;
  Kind: string;
  Before: string;
  After: string;
}

/** Server-side field names come from the Go struct (no JSON tags). */
export interface DreamReport {
  Examined: number;
  Merged: number;
  Superseded: number;
  Orphaned: number;
  Notes?: string[];
  Changes?: DreamChange[];
}

export interface ErrorEnvelope {
  error: string;
}

/** One recorded span (FR-13): model, tool, approval wait, memory, subagent. */
export interface TraceSpan {
  id: string;
  trace_id: string;
  parent_id?: string;
  kind: string;
  name?: string;
  session_id?: string;
  task_id?: string;
  agent_id?: string;
  start: string;
  end: string;
  status: string;
  error_class?: string;
  attrs?: Record<string, unknown>;
  version?: number;
}

/** Per-turn attribution rollup (matches `chyan trace` text output). */
export interface TraceSummary {
  trace_id: string;
  span_count: number;
  start: string;
  end: string;
  duration_ms: number;
  model_ms: number;
  tool_execution_ms: number;
  approval_wait_ms: number;
  memory_ms: number;
  subagent_ms: number;
  overhead_ms: number;
  tokens_in: number;
  tokens_out: number;
  cache_read_tokens: number;
  cost_usd: number;
  attempts: number;
  errors?: Record<string, number>;
}

export interface TraceTurn {
  summary: TraceSummary;
  spans: TraceSpan[];
}

export interface TraceResponse {
  turns: TraceTurn[];
}

// SSE event names (event: <name>) from the api package.
export type SseEventName =
  | 'task.started'
  | 'task.output'
  | 'task.progress'
  | 'task.done'
  | 'approval.requested'
  | 'approval.resolved'
  | 'cron.run';
