// Trace span persistence (FR-13): metadata only, redacted before it reaches
// this layer. Span rows are append-only; pruning is explicit.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"chyan.tech/chyan/internal/trace"
)

var _ trace.Sink = (*SQLite)(nil)

// SpanQuery selects spans for the trace viewer. Empty fields are ignored;
// Limit <= 0 means no limit (callers still pass a sane default).
type SpanQuery struct {
	TraceID   string
	SessionID string
	TaskID    string
	Limit     int
	// Newest reverses the order so a Limit keeps the most recent spans. The
	// viewer wants the latest turn of a long session; the CLI wants the whole
	// window in chronological order (Newest=false).
	Newest bool
}

// AppendSpan implements trace.Sink.
func (s *SQLite) AppendSpan(ctx context.Context, sp trace.Span) error {
	if sp.ID == "" || sp.TraceID == "" {
		return errors.New("store: trace span needs id and trace id")
	}
	if sp.Version == 0 {
		sp.Version = trace.SchemaVersion
	}
	attrs, err := json.Marshal(sp.Attrs)
	if err != nil {
		return fmt.Errorf("store: encode span attrs: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO trace_span(
			id, trace_id, parent_id, kind, name, session_id, task_id,
			agent_id, parent_agent_id, start_at, end_at, duration_ms,
			status, error_class, attrs, version)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sp.ID, sp.TraceID, sp.ParentID, string(sp.Kind), sp.Name,
		sp.SessionID, sp.TaskID, sp.AgentID, sp.ParentAgentID,
		formatTime(sp.Start), formatTime(sp.End), sp.Duration().Milliseconds(),
		string(sp.Status), sp.ErrorClass, string(attrs), sp.Version)
	if err != nil {
		return fmt.Errorf("store: append trace span: %w", err)
	}
	return nil
}

// ListSpans returns matching spans ordered by start time.
func (s *SQLite) ListSpans(ctx context.Context, q SpanQuery) ([]trace.Span, error) {
	query := `SELECT id, trace_id, parent_id, kind, name, session_id, task_id,
		agent_id, parent_agent_id, start_at, end_at, status, error_class, attrs, version
		FROM trace_span WHERE 1=1`
	args := make([]any, 0, 4)
	if q.TraceID != "" {
		query += " AND trace_id = ?"
		args = append(args, q.TraceID)
	}
	if q.SessionID != "" {
		query += " AND session_id = ?"
		args = append(args, q.SessionID)
	}
	if q.TaskID != "" {
		query += " AND task_id = ?"
		args = append(args, q.TaskID)
	}
	if q.Newest {
		// DESC so a LIMIT drops the oldest rows instead of the newest turn;
		// callers that render chronologically re-sort the page.
		query += " ORDER BY start_at DESC, id DESC"
	} else {
		query += " ORDER BY start_at, id"
	}
	if q.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, q.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list trace spans: %w", err)
	}
	defer rows.Close()
	var out []trace.Span
	for rows.Next() {
		var (
			sp       trace.Span
			kind     string
			status   string
			start    string
			end      string
			rawAttrs string
		)
		if err := rows.Scan(&sp.ID, &sp.TraceID, &sp.ParentID, &kind, &sp.Name,
			&sp.SessionID, &sp.TaskID, &sp.AgentID, &sp.ParentAgentID,
			&start, &end, &status, &sp.ErrorClass, &rawAttrs, &sp.Version); err != nil {
			return nil, fmt.Errorf("store: scan trace span: %w", err)
		}
		sp.Kind = trace.Kind(kind)
		sp.Status = trace.Status(status)
		sp.Start = parseTime(start)
		sp.End = parseTime(end)
		if rawAttrs != "" {
			if err := json.Unmarshal([]byte(rawAttrs), &sp.Attrs); err != nil {
				sp.Attrs = nil // corrupt attrs must not hide the span
			}
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate trace spans: %w", err)
	}
	return out, nil
}

// PruneSpans deletes spans that ended before the cutoff. dryRun only counts.
func (s *SQLite) PruneSpans(ctx context.Context, before time.Time, dryRun bool) (int, error) {
	cutoff := formatTime(before)
	var n int64
	if dryRun {
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM trace_span WHERE end_at <> '' AND end_at < ?`, cutoff).Scan(&n); err != nil {
			return 0, fmt.Errorf("store: count trace spans: %w", err)
		}
		return int(n), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM trace_span WHERE end_at <> '' AND end_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune trace spans: %w", err)
	}
	n, _ = res.RowsAffected()
	return int(n), nil
}
