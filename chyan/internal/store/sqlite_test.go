// SQLite implementation tests: shared contract suite plus SQLite-only cases
// (migration framework, cascade delete, audit autoincrement).
package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func openSQLite(t *testing.T) (Store, func()) {
	t.Helper()
	s, err := NewSQLite(filepath.Join(t.TempDir(), "chyan.db"))
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	return s, func() { _ = s.Close() }
}

func TestSQLiteContract(t *testing.T) {
	runContractTests(t, openSQLite)
}

func TestSQLiteNewErrors(t *testing.T) {
	if _, err := NewSQLite(""); err == nil {
		t.Fatal("empty path should fail")
	}
	// A directory path fails when the first statement executes.
	if _, err := NewSQLite(t.TempDir()); err == nil {
		t.Fatal("directory path should fail")
	}
}

func TestSQLiteMigrationFramework(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "chyan.db")

	s, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	v, err := s.schemaVersion(ctx)
	if err != nil {
		t.Fatalf("schemaVersion: %v", err)
	}
	if v != len(migrations) {
		t.Fatalf("schema version: want %d, got %d", len(migrations), v)
	}
	// Each applied migration is recorded exactly once.
	rows, err := s.db.Query(`SELECT version, name FROM schema_version ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			t.Fatal(err)
		}
		count++
		if version != count {
			t.Fatalf("schema_version gap: row %d has version %d", count, version)
		}
		if name == "" {
			t.Fatal("migration name empty")
		}
	}
	rows.Close()
	if count != len(migrations) {
		t.Fatalf("schema_version rows: want %d, got %d", len(migrations), count)
	}

	// Reopen: migrations are not reapplied and data survives.
	if err := s.SetKV(ctx, "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if v, _ := s2.schemaVersion(ctx); v != len(migrations) {
		t.Fatalf("schema version after reopen: got %d", v)
	}
	if got, err := s2.GetKV(ctx, "k"); err != nil || string(got) != "v" {
		t.Fatalf("data lost after reopen: %q, %v", got, err)
	}
}

func TestSQLiteDeleteSessionCascades(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openSQLite(t)
	defer closeFn()

	if err := s.CreateSession(ctx, Session{ID: "s1", CreatedAt: baseTime, UpdatedAt: baseTime}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"m1", "m2"} {
		if err := s.AppendMessage(ctx, Message{ID: id, SessionID: "s1"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteSession(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := s.ListMessages(ctx, "s1", 0, 0); len(msgs) != 0 {
		t.Fatalf("messages should cascade on session delete: %d rows", len(msgs))
	}
}

func TestSQLiteAuditAutoincrement(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openSQLite(t)
	defer closeFn()

	if err := s.AppendAudit(ctx, AuditEntry{SessionID: "s", Action: "read"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAudit(ctx, AuditEntry{SessionID: "s", Action: "bash"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAudit(ctx, "s", 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListAudit: %v, %v", list, err)
	}
	if list[0].ID <= list[1].ID || list[1].ID == 0 {
		t.Fatalf("autoincrement ids: %v", list)
	}
}

func TestSQLiteRollupNotFound(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openSQLite(t)
	defer closeFn()

	r := s.(roller)
	if err := r.RollupChildStats(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rollup unknown parent: want ErrNotFound, got %v", err)
	}
}

func TestSQLiteHelpers(t *testing.T) {
	// Time roundtrip incl. zero values.
	if got := parseTime(formatTime(time.Time{})); !got.IsZero() {
		t.Errorf("zero time roundtrip: %v", got)
	}
	ts := time.Date(2026, 3, 4, 5, 6, 7, 8, time.UTC)
	if got := parseTime(formatTime(ts)); !got.Equal(ts) {
		t.Errorf("time roundtrip: want %v, got %v", ts, got)
	}
	if got := parseTime("not-a-time"); !got.IsZero() {
		t.Errorf("bad time should be zero, got %v", got)
	}
	if nullableTime(time.Time{}) != nil {
		t.Error("zero time should map to NULL")
	}
	if nullableTime(ts) == nil {
		t.Error("non-zero time should map to string")
	}
	if boolInt(true) != 1 || boolInt(false) != 0 {
		t.Error("boolInt mismatch")
	}
	if auditID(0) != nil || auditID(7) != int64(7) {
		t.Error("auditID mismatch")
	}
	// Truncation flag ORs with the caller-provided flag.
	if _, truncated := truncateContent([]byte("x"), true); !truncated {
		t.Error("pre-set truncated flag lost")
	}
}

// TestMigrationSnapshotsBeforeUpgrade checks the forward-only upgrade path:
// pending migrations copy the DB first, and a failing migration keeps the
// process from starting with a half-applied schema.
func TestMigrationSnapshotsBeforeUpgrade(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "chyan.db")

	// Build a current-version database.
	s, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	if err := s.CreateSession(ctx, Session{ID: "s1", CreatedAt: baseTime, UpdatedAt: baseTime}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Pretend the next release adds a broken migration.
	orig := migrations
	migrations = append(append([]migration(nil), orig...), migration{
		name:  "broken test migration",
		stmts: []string{`ALTER TABLE session ADD COLUMN no_such_column TEXT NOT NULL DEFAULT ''`, `BROKEN SQL`},
	})
	t.Cleanup(func() { migrations = orig })

	if _, err := NewSQLite(path); err == nil {
		t.Fatal("broken migration must fail to open")
	}
	migrations = orig // later opens must not replay the broken step

	// A pre-migration snapshot exists and still opens cleanly.
	matches, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("pre-migration backups = %v, want 1", matches)
	}
	snap, err := NewSQLite(matches[0])
	if err != nil {
		t.Fatalf("backup must be a valid database: %v", err)
	}
	defer snap.Close()
	if _, err := snap.GetSession(ctx, "s1"); err != nil {
		t.Fatalf("backup lost data: %v", err)
	}

	// The original DB is still at the previous version and opens again.
	s2, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("reopen after restoring migrations: %v", err)
	}
	defer s2.Close()
}

// TestSQLiteMaintain: VACUUM/REINDEX/ANALYZE keep the database usable.
func TestSQLiteMaintain(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "chyan.db")
	s, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	defer s.Close()

	if err := s.CreateSession(ctx, Session{ID: "keep", CreatedAt: baseTime, UpdatedAt: baseTime}); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`[{"type":"text","text":"filler"}]`)
	for i := 0; i < 50; i++ {
		if err := s.AppendMessage(ctx, Message{
			ID: fmt.Sprintf("m%d", i), SessionID: "keep", Content: payload, CreatedAt: baseTime,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if _, err := s.GetSession(ctx, "keep"); err != nil {
		t.Fatalf("session lost after maintenance: %v", err)
	}
	if msgs, err := s.ListMessages(ctx, "keep", 0, 0); err != nil || len(msgs) != 50 {
		t.Fatalf("messages after maintenance = %d, %v", len(msgs), err)
	}
}
