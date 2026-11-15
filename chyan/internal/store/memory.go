// Pure in-memory implementation of Store for tests and ephemeral runs.
package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ChildStatRoller is optionally implemented by stores to accumulate child
// session cost/tokens into the parent (B2).
type ChildStatRoller interface {
	RollupChildStats(ctx context.Context, parentID string) error
}

// memStore is the in-memory Store. Safe for concurrent use.
type memStore struct {
	mu       sync.Mutex
	sessions map[string]Session
	messages map[string][]Message // keyed by session ID, append order
	tasks    map[string]Task
	audits   []AuditEntry
	kv       map[string][]byte
	nextID   int64
}

// NewMemory returns an empty in-memory Store.
func NewMemory() Store {
	return &memStore{
		sessions: map[string]Session{},
		messages: map[string][]Message{},
		tasks:    map[string]Task{},
		kv:       map[string][]byte{},
	}
}

// --- sessions ---

// RollupChildStats mirrors the SQLite behaviour (B2): accumulate each direct
// child's cumulative stats into the parent. Each call accumulates.
func (m *memStore) RollupChildStats(_ context.Context, parentID string) error {
	if parentID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	parent, ok := m.sessions[parentID]
	if !ok {
		return ErrNotFound
	}
	for _, s := range m.sessions {
		if s.ParentID == parentID {
			parent.CostUSD += s.CostUSD
			parent.TokensIn += s.TokensIn
			parent.TokensOut += s.TokensOut
			parent.CacheReadTokens += s.CacheReadTokens
			parent.ReasoningTokens += s.ReasoningTokens
		}
	}
	parent.UpdatedAt = time.Now()
	m.sessions[parentID] = parent
	return nil
}

func (m *memStore) CreateSession(_ context.Context, s Session) error {
	if s.ID == "" {
		return errors.New("store: session id is empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[s.ID]; ok {
		return errors.New("store: create session " + s.ID + ": already exists")
	}
	if s.Status == "" {
		s.Status = SessionStatusActive
	}
	m.sessions[s.ID] = s
	return nil
}

func (m *memStore) GetSession(_ context.Context, id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	return s, nil
}

// ListSessions returns sessions ordered by most recently updated first.
func (m *memStore) ListSessions(_ context.Context, limit, offset int) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return page(out, limit, offset), nil
}

func (m *memStore) UpdateSession(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[s.ID]; !ok {
		return ErrNotFound
	}
	if s.Status == "" {
		s.Status = SessionStatusActive
	}
	m.sessions[s.ID] = s
	return nil
}

func (m *memStore) DeleteSession(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[id]; !ok {
		return ErrNotFound
	}
	delete(m.sessions, id)
	delete(m.messages, id)
	return nil
}

// --- messages ---

func (m *memStore) AppendMessage(_ context.Context, msg Message) error {
	if msg.ID == "" {
		return errors.New("store: message id is empty")
	}
	if msg.SessionID == "" {
		return errors.New("store: message session id is empty")
	}
	content, truncated := truncateContent(msg.Content, msg.Truncated)
	msg.Content = content
	msg.Truncated = truncated
	m.mu.Lock()
	defer m.mu.Unlock()
	// SQLite declares message.id as a global primary key, not per session.
	for _, messages := range m.messages {
		for _, existing := range messages {
			if existing.ID == msg.ID {
				return fmt.Errorf("store: append message %s: duplicate id", msg.ID)
			}
		}
	}
	m.messages[msg.SessionID] = append(m.messages[msg.SessionID], msg)
	return nil
}

// ListMessages returns messages in insertion order: equal-timestamp messages
// (a tool call and its result share a second) must not be re-sorted by id,
// or replayed history loses the tool pairing.
func (m *memStore) ListMessages(_ context.Context, sessionID string, limit, offset int) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]Message(nil), m.messages[sessionID]...)
	return page(out, limit, offset), nil
}

// ListRecentMessages returns the last limit messages in insertion order.
func (m *memStore) ListRecentMessages(_ context.Context, sessionID string, limit int) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := m.messages[sessionID]
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return append([]Message(nil), all...), nil
}

// --- tasks ---

func (m *memStore) CreateTask(_ context.Context, t Task) error {
	if t.ID == "" {
		return errors.New("store: task id is empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[t.ID]; ok {
		return errors.New("store: create task " + t.ID + ": already exists")
	}
	m.tasks[t.ID] = t
	return nil
}

// ListTasks returns tasks, optionally filtered by status ("" = all).
func (m *memStore) ListTasks(_ context.Context, status string) ([]Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Task{}
	for _, t := range m.tasks {
		if status == "" || t.Status == status {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *memStore) UpdateTask(_ context.Context, t Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[t.ID]; !ok {
		return ErrNotFound
	}
	m.tasks[t.ID] = t
	return nil
}

// PruneAudit deletes audit rows older than before (dryRun only counts).
func (m *memStore) PruneAudit(_ context.Context, before time.Time, dryRun bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.audits {
		if e.CreatedAt.Before(before) {
			n++
		}
	}
	if dryRun || n == 0 {
		return n, nil
	}
	kept := make([]AuditEntry, 0, len(m.audits)-n)
	for _, e := range m.audits {
		if e.CreatedAt.Before(before) {
			continue
		}
		kept = append(kept, e)
	}
	m.audits = kept
	return n, nil
}

// PruneSessions deletes sessions older than before plus their messages.
func (m *memStore) PruneSessions(_ context.Context, before time.Time, dryRun bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, s := range m.sessions {
		if !s.UpdatedAt.Before(before) {
			continue
		}
		n++
		if !dryRun {
			delete(m.sessions, id)
			delete(m.messages, id)
		}
	}
	return n, nil
}

// --- audit ---

func (m *memStore) AppendAudit(_ context.Context, e AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	e.ID = m.nextID
	m.audits = append(m.audits, e)
	return nil
}

// ListAudit returns the newest audit entries for one session (limit <= 0: all).
func (m *memStore) ListAudit(_ context.Context, sessionID string, limit int) ([]AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []AuditEntry{}
	for i := len(m.audits) - 1; i >= 0; i-- {
		if m.audits[i].SessionID == sessionID {
			out = append(out, m.audits[i])
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// --- kv ---

func (m *memStore) GetKV(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.kv[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (m *memStore) SetKV(_ context.Context, key string, value []byte) error {
	if key == "" {
		return errors.New("store: kv key is empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kv[key] = append([]byte(nil), value...)
	return nil
}

func (m *memStore) Close() error { return nil }

// page applies limit/offset with the shared store contract: limit <= 0 means
// "no limit"; a negative offset is treated as 0 instead of panicking.
func page[T any](in []T, limit, offset int) []T {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(in) {
		return []T{}
	}
	in = in[offset:]
	if limit > 0 && len(in) > limit {
		in = in[:limit]
	}
	if len(in) == 0 {
		return []T{}
	}
	return in
}
