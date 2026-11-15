// Package store defines persistence contracts backed by SQLite
// (~/.chyan/chyan.db): sessions, messages, tasks, memories,
// audit, kv. Shared knowledge lives in .agents/ dirs, not here.
package store

import (
	"context"
	"time"
)

// Session status values. A run cut short by the user marks the session
// interrupted; the next successful turn resets it to active.
const (
	SessionStatusActive      = "active"
	SessionStatusInterrupted = "interrupted"
)

// Session is one conversation thread. Sub-agents link to parents via ParentID.
type Session struct {
	ID        string
	ParentID  string // empty for root sessions; child of a sub-agent dispatch
	Agent     string // agent definition name ("" = default)
	Status    string // active | interrupted ("" = active in legacy rows)
	Title     string
	Model     string
	CostUSD   float64 // cumulative, sub-agent cost rolls up to parent
	TokensIn  int
	TokensOut int
	// CacheReadTokens and ReasoningTokens break out the prompt cache hits and
	// thinking tokens contained in TokensIn/TokensOut.
	CacheReadTokens int
	ReasoningTokens int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Message is a persisted conversation row. Content is a JSON blob of parts;
// oversized payloads are stored truncated with Truncated set.
type Message struct {
	ID        string
	SessionID string
	Role      string
	Content   []byte // JSON parts array
	Truncated bool
	CreatedAt time.Time
}

// Task is an asynchronous daemon job: a scheduled cron entry, or a one-shot
// background dispatch launched via the daemon API (from PWA/IM/CLI).
// Synchronous sub-agent dispatches are sessions (ParentID), not tasks.
type Task struct {
	ID        string
	SessionID string
	Kind      string // "cron" (scheduled) | "dispatch" (one-shot background)
	Schedule  string // cron expression, empty for one-shot
	Status    string // "pending" | "running" | "done" | "failed" | "interrupted"
	LastRunAt time.Time
	CreatedAt time.Time
}

// Memory is the in-memory model of one long-term memory entry (tier 3 of the
// memory system). It is NOT a persisted DB row: the truth source is a
// markdown file under .agents/memory/<kind>/ with SKILL.md style frontmatter,
// and persistence is owned by internal/memory (FileStore). This type is
// shared as a data model only. Superseded entries are marked, never deleted
// (dream consolidation chain).
type Memory struct {
	ID          string
	Path        string // markdown file path (truth source)
	Domain      string // "global" | "project" | "project-local"
	ProjectPath string // repo path; set when Domain is "project" or "project-local", empty for global
	Kind        string // "fact" | "kb" | "model" | "directive"
	Type        string // semantic | episodic | procedural (facts only)
	Name        string // frontmatter name
	Keywords    []string
	Triggers    []string // lesson recall cues: scenario keywords, matched before doing the thing
	Content     string   // body text
	Source      string   // "explicit" | "dream" | "migrated:hindsight"
	Status      string   // "active" | "superseded"
	Supersedes  string   // id of the memory this one replaces
	Pinned      bool
	LastUsedAt  time.Time
	CreatedAt   time.Time
}

// AuditEntry records one gated operation for traceability.
type AuditEntry struct {
	ID         int64
	SessionID  string
	Action     string // bash/edit/read/web/...
	Resource   string
	Effect     string // allow/ask/deny
	ReviewMode string // human/ai/full
	Verdict    string // ai reviewer verdict or human approve/deny reason
	Approved   bool
	Actor      string // "user" | "subagent:<name>"
	CreatedAt  time.Time
}

// Store is the persistence boundary. Implementations use SQLite; tests use
// an in-memory implementation of the same interface.
type Store interface {
	// Sessions
	CreateSession(ctx context.Context, s Session) error
	GetSession(ctx context.Context, id string) (Session, error)
	ListSessions(ctx context.Context, limit, offset int) ([]Session, error)
	UpdateSession(ctx context.Context, s Session) error
	DeleteSession(ctx context.Context, id string) error

	// Messages
	AppendMessage(ctx context.Context, m Message) error
	ListMessages(ctx context.Context, sessionID string, limit, offset int) ([]Message, error)
	// ListRecentMessages returns up to the last limit messages of a session
	// in creation order (append order, not reversed).
	ListRecentMessages(ctx context.Context, sessionID string, limit int) ([]Message, error)

	// Tasks
	CreateTask(ctx context.Context, t Task) error
	ListTasks(ctx context.Context, status string) ([]Task, error)
	UpdateTask(ctx context.Context, t Task) error

	// Memories are NOT persisted here: md files are the truth source and
	// are owned by internal/memory (FileStore). This store keeps session
	// data only (session/message/task/audit/kv).

	// Audit
	AppendAudit(ctx context.Context, e AuditEntry) error
	ListAudit(ctx context.Context, sessionID string, limit int) ([]AuditEntry, error)
	// PruneAudit deletes audit rows older than before and reports how many
	// match (dryRun only counts).
	PruneAudit(ctx context.Context, before time.Time, dryRun bool) (int, error)

	// PruneSessions deletes sessions whose updated_at is older than before,
	// together with their messages. dryRun only counts. Child sessions are
	// pruned independently (their parent_id may then dangle).
	PruneSessions(ctx context.Context, before time.Time, dryRun bool) (int, error)

	// Key-value runtime snapshot (context meta, migration versions)
	GetKV(ctx context.Context, key string) ([]byte, error)
	SetKV(ctx context.Context, key string, value []byte) error

	Close() error
}
