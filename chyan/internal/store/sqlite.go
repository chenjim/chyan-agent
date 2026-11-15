// SQLite implementation of the Store contract. The database lives in a
// single file (typically ~/.chyan/chyan.db; the caller resolves the path).
// Memory persistence is NOT here: md files are the truth source, owned by
// internal/memory. This store keeps session data only.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// MaxMessageBytes is the message content persistence cap (tool_output_max_bytes,
// architecture 4.1: store truncated content + Truncated flag as truth source).
const MaxMessageBytes = 20000

// SQLite implements Store over a single SQLite file.
type SQLite struct {
	db *sql.DB
	// path is the database file; migrations snapshot it before upgrading.
	path string

	// serializes writes: SQLite has one writer anyway; batching under one
	// lock keeps rollup read-modify-write atomic across goroutines.
	mu sync.Mutex
}

// migration is one ordered schema step. Append only; applied versions are
// recorded in schema_version and never reapplied.
type migration struct {
	name  string
	stmts []string
}

// migrations runs in order. New entries only append (forward-only).
var migrations = []migration{
	{
		name: "core tables",
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS schema_version (
				version INTEGER PRIMARY KEY,
				name TEXT NOT NULL,
				applied_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS session (
				id TEXT PRIMARY KEY,
				parent_id TEXT NOT NULL DEFAULT '',
				agent TEXT NOT NULL DEFAULT '',
				title TEXT NOT NULL DEFAULT '',
				model TEXT NOT NULL DEFAULT '',
				cost_usd REAL NOT NULL DEFAULT 0,
				tokens_in INTEGER NOT NULL DEFAULT 0,
				tokens_out INTEGER NOT NULL DEFAULT 0,
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_session_parent ON session(parent_id)`,
			`CREATE TABLE IF NOT EXISTS message (
				id TEXT PRIMARY KEY,
				session_id TEXT NOT NULL,
				content BLOB NOT NULL,
				truncated INTEGER NOT NULL DEFAULT 0,
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_message_session ON message(session_id)`,
			`CREATE TABLE IF NOT EXISTS task (
				id TEXT PRIMARY KEY,
				session_id TEXT NOT NULL DEFAULT '',
				kind TEXT NOT NULL,
				schedule TEXT NOT NULL DEFAULT '',
				status TEXT NOT NULL,
				last_run_at TEXT,
				created_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS audit (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				session_id TEXT NOT NULL DEFAULT '',
				action TEXT NOT NULL,
				resource TEXT NOT NULL DEFAULT '',
				effect TEXT NOT NULL,
				review_mode TEXT NOT NULL DEFAULT '',
				verdict TEXT NOT NULL DEFAULT '',
				approved INTEGER NOT NULL DEFAULT 0,
				actor TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_audit_session ON audit(session_id)`,
			`CREATE TABLE IF NOT EXISTS kv (
				key TEXT PRIMARY KEY,
				value BLOB NOT NULL
			)`,
		},
	},
	{
		name: "message role column",
		stmts: []string{
			// role was missing from the first schema: without it a replayed
			// session loses who said what and tool pairs look orphaned.
			`ALTER TABLE message ADD COLUMN role TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		name: "session usage detail columns",
		stmts: []string{
			// Cached prompt tokens and thinking tokens are reported by
			// OpenAI-compatible usage blocks; keep them separate from the
			// billing totals so cache hit rate is visible.
			`ALTER TABLE session ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE session ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		name: "session status column",
		stmts: []string{
			// Run status for the session list: a turn cut short by the user
			// marks it interrupted; the next successful turn resets it.
			`ALTER TABLE session ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`,
		},
	},
	{
		name: "trace spans",
		stmts: []string{
			// Local trace metadata (FR-13): one row per span. Payloads (prompt,
			// tool input/output, memory bodies) are never stored here; attrs
			// hold redacted, size-bounded metadata.
			`CREATE TABLE IF NOT EXISTS trace_span (
				id TEXT PRIMARY KEY,
				trace_id TEXT NOT NULL,
				parent_id TEXT NOT NULL DEFAULT '',
				kind TEXT NOT NULL,
				name TEXT NOT NULL DEFAULT '',
				session_id TEXT NOT NULL DEFAULT '',
				task_id TEXT NOT NULL DEFAULT '',
				agent_id TEXT NOT NULL DEFAULT '',
				parent_agent_id TEXT NOT NULL DEFAULT '',
				start_at TEXT NOT NULL,
				end_at TEXT NOT NULL,
				duration_ms INTEGER NOT NULL DEFAULT 0,
				status TEXT NOT NULL DEFAULT '',
				error_class TEXT NOT NULL DEFAULT '',
				attrs TEXT NOT NULL DEFAULT '{}',
				version INTEGER NOT NULL DEFAULT 1
			)`,
			`CREATE INDEX IF NOT EXISTS idx_trace_span_trace ON trace_span(trace_id, start_at)`,
			`CREATE INDEX IF NOT EXISTS idx_trace_span_session ON trace_span(session_id, start_at)`,
		},
	},
}

// NewSQLite opens (creating if needed) the SQLite file at path and runs
// pending migrations.
func NewSQLite(path string) (*SQLite, error) {
	if path == "" {
		return nil, errors.New("store: sqlite path is empty")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	// Single writer connection: serialized access, no SQLITE_BUSY surprises.
	db.SetMaxOpenConns(1)
	s := &SQLite{db: db, path: path}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate applies pending migrations in order under a write lock. Each
// migration and its schema_version row commit together: a failure never
// leaves a half-applied migration that a restart would replay into a
// duplicate-column error.
func (s *SQLite) migrate(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, migrations[0].stmts[0]); err != nil {
		return fmt.Errorf("store: init schema_version: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	// Upgrade path: snapshot the database before touching it. A failed
	// migration then leaves both the backup and the original file intact
	// (the caller refuses to start, and the user can restore by hand).
	if current > 0 && current < len(migrations) {
		if err := s.snapshotBeforeMigrate(ctx); err != nil {
			return err
		}
	}
	for i := current; i < len(migrations); i++ {
		if err := s.applyMigration(ctx, i, migrations[i]); err != nil {
			return err
		}
	}
	return nil
}

// snapshotBeforeMigrate writes a consistent copy next to the database using
// VACUUM INTO (safe while the connection is open, unlike a raw file copy).
func (s *SQLite) snapshotBeforeMigrate(ctx context.Context) error {
	dst := fmt.Sprintf("%s.bak-%s", s.path, time.Now().Format("20060102-150405"))
	if _, err := os.Stat(dst); err == nil {
		dst = fmt.Sprintf("%s.%d", dst, time.Now().UnixNano())
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("store: pre-migration backup: %w", err)
	}
	return nil
}

// applyMigration runs one migration and records its version atomically.
func (s *SQLite) applyMigration(ctx context.Context, i int, m migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %d (%s): %w", i+1, m.name, err)
	}
	defer tx.Rollback()
	for _, stmt := range m.stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: migration %d (%s): %w", i+1, m.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_version(version, name, applied_at) VALUES(?, ?, ?)`,
		i+1, m.name, formatTime(time.Now())); err != nil {
		return fmt.Errorf("store: record migration %d: %w", i+1, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %d (%s): %w", i+1, m.name, err)
	}
	return nil
}

// schemaVersion reports the highest applied migration version.
func (s *SQLite) schemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v)
	return v, err
}

// --- sessions ---

// RollupChildStats adds the cumulative cost/tokens of each direct child
// session into its parent's counters (B2). Call after child stats change;
// the operation is idempotent only in structure — repeated calls accumulate,
// so callers invoke it once per stats update.
func (s *SQLite) RollupChildStats(ctx context.Context, parentID string) error {
	if parentID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE session SET
			cost_usd = cost_usd + (SELECT COALESCE(SUM(cost_usd), 0) FROM session WHERE parent_id = ?),
			tokens_in = tokens_in + (SELECT COALESCE(SUM(tokens_in), 0) FROM session WHERE parent_id = ?),
			tokens_out = tokens_out + (SELECT COALESCE(SUM(tokens_out), 0) FROM session WHERE parent_id = ?),
			cache_read_tokens = cache_read_tokens + (SELECT COALESCE(SUM(cache_read_tokens), 0) FROM session WHERE parent_id = ?),
			reasoning_tokens = reasoning_tokens + (SELECT COALESCE(SUM(reasoning_tokens), 0) FROM session WHERE parent_id = ?),
			updated_at = ?
		WHERE id = ?`,
		parentID, parentID, parentID, parentID, parentID, formatTime(time.Now()), parentID)
	if err != nil {
		return fmt.Errorf("store: rollup child stats: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: session %s: %w", parentID, ErrNotFound)
	}
	return nil
}

func (s *SQLite) CreateSession(ctx context.Context, sess Session) error {
	if sess.ID == "" {
		return errors.New("store: session id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session(id, parent_id, agent, title, model, cost_usd, tokens_in, tokens_out,
			cache_read_tokens, reasoning_tokens, status, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.ParentID, sess.Agent, sess.Title, sess.Model,
		sess.CostUSD, sess.TokensIn, sess.TokensOut,
		sess.CacheReadTokens, sess.ReasoningTokens, sessionStatus(sess.Status),
		formatTime(sess.CreatedAt), formatTime(sess.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: create session %s: %w", sess.ID, err)
	}
	return nil
}

func (s *SQLite) GetSession(ctx context.Context, id string) (Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, parent_id, agent, title, model, cost_usd, tokens_in, tokens_out,
			cache_read_tokens, reasoning_tokens, status, created_at, updated_at
		FROM session WHERE id = ?`, id)
	return scanSession(row)
}

// ListSessions returns sessions ordered by most recently updated first.
func (s *SQLite) ListSessions(ctx context.Context, limit, offset int) ([]Session, error) {
	// julianday() compares real timestamps: the stored RFC3339Nano text
	// sorts wrongly inside one second ("...:00.5Z" < "...:00Z").
	q := `SELECT id, parent_id, agent, title, model, cost_usd, tokens_in, tokens_out,
			cache_read_tokens, reasoning_tokens, status, created_at, updated_at
		FROM session ORDER BY julianday(updated_at) DESC, id DESC`
	q, args := pageClause(q, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// pageClause appends LIMIT/OFFSET with the shared contract: limit <= 0 means
// "no limit", offset applies on its own too.
func pageClause(q string, limit, offset int) (string, []any) {
	if offset < 0 {
		offset = 0
	}
	switch {
	case limit > 0:
		return q + ` LIMIT ? OFFSET ?`, []any{limit, offset}
	case offset > 0:
		return q + ` LIMIT -1 OFFSET ?`, []any{offset}
	}
	return q, nil
}

// UpdateSession overwrites the row. Child stat rollup is explicit via
// RollupChildStats (callers own update ordering).
func (s *SQLite) UpdateSession(ctx context.Context, sess Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE session SET parent_id = ?, agent = ?, title = ?, model = ?,
			cost_usd = ?, tokens_in = ?, tokens_out = ?, cache_read_tokens = ?,
			reasoning_tokens = ?, status = ?, created_at = ?, updated_at = ?
		WHERE id = ?`,
		sess.ParentID, sess.Agent, sess.Title, sess.Model,
		sess.CostUSD, sess.TokensIn, sess.TokensOut,
		sess.CacheReadTokens, sess.ReasoningTokens, sessionStatus(sess.Status),
		formatTime(sess.CreatedAt), formatTime(sess.UpdatedAt), sess.ID)
	if err != nil {
		return fmt.Errorf("store: update session %s: %w", sess.ID, err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return fmt.Errorf("store: session %s: %w", sess.ID, ErrNotFound)
	}
	return err
}

func (s *SQLite) DeleteSession(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete session %s: %w", id, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM session WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete session %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: session %s: %w", id, ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM message WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("store: delete session %s messages: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete session %s: %w", id, err)
	}
	return nil
}

// --- messages ---

// AppendMessage stores m.Content as a JSON blob, truncated to MaxMessageBytes
// with the Truncated flag set when over the cap.
func (s *SQLite) AppendMessage(ctx context.Context, m Message) error {
	if m.ID == "" {
		return errors.New("store: message id is empty")
	}
	if m.SessionID == "" {
		return errors.New("store: message session id is empty")
	}
	content, truncated := truncateContent(m.Content, m.Truncated)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO message(id, session_id, role, content, truncated, created_at)
		VALUES(?, ?, ?, ?, ?, ?)`,
		m.ID, m.SessionID, m.Role, content, boolInt(truncated), formatTime(m.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: append message %s: %w", m.ID, err)
	}
	return nil
}

// ListMessages returns messages in creation order.
func (s *SQLite) ListMessages(ctx context.Context, sessionID string, limit, offset int) ([]Message, error) {
	q := `SELECT id, session_id, role, content, truncated, created_at
		FROM message WHERE session_id = ? ORDER BY rowid`
	q, pageArgs := pageClause(q, limit, offset)
	args := append([]any{sessionID}, pageArgs...)
	return s.queryMessages(ctx, q, args...)
}

// ListRecentMessages returns up to the last limit messages of a session in
// creation order: a DESC rowid scan takes the tail, then flips back so the
// result stays append-ordered. limit <= 0 returns the whole session.
func (s *SQLite) ListRecentMessages(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = -1 // SQLite "no limit"
	}
	q := `SELECT id, session_id, role, content, truncated, created_at
		FROM message WHERE session_id = ? ORDER BY rowid DESC LIMIT ?`
	rows, err := s.queryMessages(ctx, q, sessionID, limit)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

// queryMessages runs a message SELECT and scans the rows.
func (s *SQLite) queryMessages(ctx context.Context, q string, args ...any) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list messages: %w", err)
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var truncated int
		var created string
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &truncated, &created); err != nil {
			return nil, fmt.Errorf("store: scan message: %w", err)
		}
		m.Truncated = truncated != 0
		m.CreatedAt = parseTime(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// --- tasks ---

func (s *SQLite) CreateTask(ctx context.Context, t Task) error {
	if t.ID == "" {
		return errors.New("store: task id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO task(id, session_id, kind, schedule, status, last_run_at, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.SessionID, t.Kind, t.Schedule, t.Status,
		nullableTime(t.LastRunAt), formatTime(t.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: create task %s: %w", t.ID, err)
	}
	return nil
}

// ListTasks returns tasks, optionally filtered by status ("" = all).
func (s *SQLite) ListTasks(ctx context.Context, status string) ([]Task, error) {
	q := `SELECT id, session_id, kind, schedule, status, last_run_at, created_at FROM task`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks: %w", err)
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLite) UpdateTask(ctx context.Context, t Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE task SET session_id = ?, kind = ?, schedule = ?, status = ?, last_run_at = ?
		WHERE id = ?`,
		t.SessionID, t.Kind, t.Schedule, t.Status, nullableTime(t.LastRunAt), t.ID)
	if err != nil {
		return fmt.Errorf("store: update task %s: %w", t.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: task %s: %w", t.ID, ErrNotFound)
	}
	return nil
}

// --- audit ---

func (s *SQLite) AppendAudit(ctx context.Context, e AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit(id, session_id, action, resource, effect, review_mode, verdict, approved, actor, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		auditID(e.ID), e.SessionID, e.Action, e.Resource, e.Effect, e.ReviewMode,
		e.Verdict, boolInt(e.Approved), e.Actor, formatTime(e.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: append audit: %w", err)
	}
	return nil
}

// ListAudit returns the newest audit entries for one session (limit <= 0: all).
func (s *SQLite) ListAudit(ctx context.Context, sessionID string, limit int) ([]AuditEntry, error) {
	q := `SELECT id, session_id, action, resource, effect, review_mode, verdict, approved, actor, created_at
		FROM audit WHERE session_id = ? ORDER BY id DESC`
	args := []any{sessionID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var approved int
		var created string
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Action, &e.Resource, &e.Effect,
			&e.ReviewMode, &e.Verdict, &approved, &e.Actor, &created); err != nil {
			return nil, fmt.Errorf("store: scan audit: %w", err)
		}
		e.Approved = approved != 0
		e.CreatedAt = parseTime(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Maintain compacts the database file and refreshes query planning data
// (VACUUM + REINDEX + ANALYZE). It is the manual maintenance entry behind
// `chyan vacuum`; SQLite runs it as a whole-file rewrite, so it is slow on
// large databases and must not run while a write transaction is open.
func (s *SQLite) Maintain(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	steps := []struct{ name, stmt string }{
		{"vacuum", `VACUUM`},
		{"reindex", `REINDEX`},
		{"analyze", `ANALYZE`},
	}
	for _, step := range steps {
		if _, err := s.db.ExecContext(ctx, step.stmt); err != nil {
			return fmt.Errorf("store: %s: %w", step.name, err)
		}
	}
	return nil
}

// PruneAudit deletes audit rows older than before (dryRun only counts).
func (s *SQLite) PruneAudit(ctx context.Context, before time.Time, dryRun bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	const where = ` FROM audit WHERE julianday(created_at) < julianday(?)`
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+where, formatTime(before)).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count audit: %w", err)
	}
	if dryRun || n == 0 {
		return n, nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE`+where, formatTime(before)); err != nil {
		return 0, fmt.Errorf("store: prune audit: %w", err)
	}
	return n, nil
}

// PruneSessions deletes sessions older than before plus their messages.
func (s *SQLite) PruneSessions(ctx context.Context, before time.Time, dryRun bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	const where = ` FROM session WHERE julianday(updated_at) < julianday(?)`
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+where, formatTime(before)).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count sessions: %w", err)
	}
	if dryRun || n == 0 {
		return n, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: prune sessions: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM message WHERE session_id IN (SELECT id`+where+`)`, formatTime(before)); err != nil {
		return 0, fmt.Errorf("store: prune session messages: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE`+where, formatTime(before)); err != nil {
		return 0, fmt.Errorf("store: prune sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: prune sessions: %w", err)
	}
	return n, nil
}

// --- kv ---
// GetKV returns ErrNotFound when the key does not exist.
func (s *SQLite) GetKV(ctx context.Context, key string) ([]byte, error) {
	var value []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: kv %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get kv %q: %w", key, err)
	}
	return value, nil
}

func (s *SQLite) SetKV(ctx context.Context, key string, value []byte) error {
	if key == "" {
		return errors.New("store: kv key is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO kv(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("store: set kv %q: %w", key, err)
	}
	return nil
}

func (s *SQLite) Close() error {
	return s.db.Close()
}

// --- helpers ---

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanSession(row scanner) (Session, error) {
	var sess Session
	var created, updated string
	err := row.Scan(&sess.ID, &sess.ParentID, &sess.Agent, &sess.Title, &sess.Model,
		&sess.CostUSD, &sess.TokensIn, &sess.TokensOut,
		&sess.CacheReadTokens, &sess.ReasoningTokens, &sess.Status, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: scan session: %w", err)
	}
	sess.CreatedAt = parseTime(created)
	sess.UpdatedAt = parseTime(updated)
	return sess, nil
}

// sessionStatus normalizes the session status written to SQLite: an empty
// value (legacy callers) means active.
func sessionStatus(status string) string {
	if status == "" {
		return SessionStatusActive
	}
	return status
}

func scanTask(row scanner) (Task, error) {
	var t Task
	var lastRun sql.NullString
	var created string
	err := row.Scan(&t.ID, &t.SessionID, &t.Kind, &t.Schedule, &t.Status, &lastRun, &created)
	if err != nil {
		return Task{}, fmt.Errorf("store: scan task: %w", err)
	}
	if lastRun.Valid {
		t.LastRunAt = parseTime(lastRun.String)
	}
	t.CreatedAt = parseTime(created)
	return t, nil
}

// truncateContent caps content at MaxMessageBytes and ORs in the caller flag.
// Nil content normalizes to an empty blob (NOT NULL column).
func truncateContent(content []byte, alreadyTruncated bool) ([]byte, bool) {
	truncated := alreadyTruncated
	if content == nil {
		content = []byte{}
	}
	if len(content) > MaxMessageBytes {
		// Byte-level cut may split a UTF-8 rune; acceptable for a
		// truncation marker payload.
		content = content[:MaxMessageBytes]
		truncated = true
	}
	return content, truncated
}

// formatTime stores RFC3339 with nanoseconds; zero time maps to "".
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// nullableTime maps zero time to NULL (task.last_run_at).
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatTime(t)
}

// parseTime inverts formatTime; unparsable values yield zero time.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// auditID maps 0 to nil so SQLite assigns an autoincrement id.
func auditID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
