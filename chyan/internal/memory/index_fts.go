package memory

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// FTS5 backend of the memory index. Documents are tokenized at the
// application layer (CJK bigram + English words + CJK unigrams) and stored
// space-joined into one fts5 column so the unicode61 tokenizer only sees
// whitespace-separated terms. A companion idx_doc table carries the ranking
// metadata fts5 does not hold (kind, status, pinned, mtime, summary). Rows
// are keyed by file path: the markdown file is the truth source identity,
// and ids are only unique per domain.

const ftsSchema = `
CREATE TABLE IF NOT EXISTS idx_meta (k TEXT PRIMARY KEY, v TEXT);
CREATE TABLE IF NOT EXISTS idx_doc (
        path TEXT PRIMARY KEY,
        id TEXT NOT NULL,
        domain TEXT NOT NULL,
        kind TEXT NOT NULL,
        name TEXT NOT NULL,
        keywords TEXT,
        triggers TEXT,
        summary TEXT,
        status TEXT NOT NULL,
        pinned INTEGER NOT NULL DEFAULT 0,
        created_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_doc_id ON idx_doc(id);
CREATE VIRTUAL TABLE IF NOT EXISTS idx_fts USING fts5(tok);
`

const ftsSchemaVersion = "1"

var sqliteMagic = []byte("SQLite format 3\x00")

// ftsBackend implements backend over one SQLite file.
type ftsBackend struct {
	db *sql.DB
}

// fts5Available probes the linked SQLite build for fts5 support (verified at
// runtime on modernc.org/sqlite).
func fts5Available() bool {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return false
	}
	defer db.Close()
	_, err = db.Exec("CREATE VIRTUAL TABLE fts5probe USING fts5(x)")
	return err == nil
}

// newFTSBackend opens the cache file, discarding a file written by the
// fallback backend (it is always derivable via Rebuild).
func newFTSBackend(path string) (backend, error) {
	if err := discardForeignCache(path, true); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	b := &ftsBackend{db: db}
	if err := b.init(); err != nil {
		db.Close()
		return nil, err
	}
	return b, nil
}

// init creates the schema, dropping a stale schema version first: the cache
// is always derivable via Rebuild.
func (b *ftsBackend) init() error {
	var v string
	err := b.db.QueryRow("SELECT v FROM idx_meta WHERE k = 'schema'").Scan(&v)
	if err == nil && v == ftsSchemaVersion {
		return nil
	}
	// Anything else (missing table, wrong version, corrupted meta)
	// falls through to a full cache reset.
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS idx_fts",
		"DROP TABLE IF EXISTS idx_doc",
		"DROP TABLE IF EXISTS idx_meta",
	} {
		if _, err := b.db.Exec(stmt); err != nil {
			return fmt.Errorf("memory: index reset: %w", err)
		}
	}
	if _, err := b.db.Exec(ftsSchema); err != nil {
		return fmt.Errorf("memory: index schema: %w", err)
	}
	_, err = b.db.Exec("INSERT INTO idx_meta(k, v) VALUES ('schema', ?)", ftsSchemaVersion)
	return err
}

func (b *ftsBackend) upsert(docs []indexDoc) error {
	return inTx(b.db, func(tx *sql.Tx) error {
		for _, d := range docs {
			if err := ftsUpsertOne(tx, d); err != nil {
				return err
			}
		}
		return nil
	})
}

// ftsUpsertOne replaces the row of d's file path, keeping rows of other
// domains intact (ids are only unique per domain).
func ftsUpsertOne(tx *sql.Tx, d indexDoc) error {
	key := docKey(d)
	if err := ftsDeleteWhere(tx, "path = ?", key); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO idx_doc(path, id, domain, kind, name, keywords, triggers, summary, status, pinned, created_ms)
                VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		key, d.ID, d.Domain, d.Kind, d.Name,
		strings.Join(d.Keywords, "\x00"), strings.Join(d.Triggers, "\x00"),
		d.Summary, d.Status, boolInt(d.Pinned), d.CreatedAt.UnixMilli())
	if err != nil {
		return err
	}
	rowid, err := lastRowID(tx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO idx_fts(rowid, tok) VALUES (?, ?)`, rowid, joinTokens(d))
	return err
}

func (b *ftsBackend) remove(ids []string) error {
	return inTx(b.db, func(tx *sql.Tx) error {
		for _, id := range ids {
			if err := ftsDeleteWhere(tx, "id = ?", id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (b *ftsBackend) replaceUnder(root string, docs []indexDoc) error {
	return inTx(b.db, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT path FROM idx_doc`)
		if err != nil {
			return err
		}
		var stale []string
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				rows.Close()
				return err
			}
			if underRoot(path, root) {
				stale = append(stale, path)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, path := range stale {
			if err := ftsDeleteWhere(tx, "path = ?", path); err != nil {
				return err
			}
		}
		for _, d := range docs {
			if err := ftsUpsertOne(tx, d); err != nil {
				return err
			}
		}
		return nil
	})
}

// ftsDeleteWhere removes matching rows plus their fts rows.
func ftsDeleteWhere(tx *sql.Tx, where string, args ...any) error {
	rows, err := tx.Query(`SELECT path FROM idx_doc WHERE `+where, args...)
	if err != nil {
		return err
	}
	var keys []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, path)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, key := range keys {
		if err := ftsDeleteOne(tx, key); err != nil {
			return err
		}
	}
	return nil
}

func ftsDeleteOne(tx *sql.Tx, key string) error {
	var rowid int64
	err := tx.QueryRow(`SELECT rowid FROM idx_doc WHERE path = ?`, key).Scan(&rowid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM idx_fts WHERE rowid = ?`, rowid); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM idx_doc WHERE path = ?`, key)
	return err
}

// match runs one AND query over the tokenized column with bm25 ranking.
func (b *ftsBackend) match(queryTokens []string) ([]matchResult, error) {
	if len(queryTokens) == 0 {
		return nil, nil
	}
	expr := make([]string, len(queryTokens))
	for i, t := range queryTokens {
		expr[i] = quoteFTS(t)
	}
	rows, err := b.db.Query(`SELECT d.path, d.id, d.domain, d.kind, d.name, d.keywords, d.triggers,
                        d.summary, d.status, d.pinned, d.created_ms, bm25(idx_fts)
                FROM idx_fts JOIN idx_doc d ON d.rowid = idx_fts.rowid
                WHERE idx_fts MATCH ? ORDER BY bm25(idx_fts)`, strings.Join(expr, " AND "))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []matchResult
	for rows.Next() {
		var (
			d         indexDoc
			kw, trg   string
			pinned    int64
			createdMS int64
			rank      float64
		)
		if err := rows.Scan(&d.Path, &d.ID, &d.Domain, &d.Kind, &d.Name, &kw, &trg,
			&d.Summary, &d.Status, &pinned, &createdMS, &rank); err != nil {
			return nil, err
		}
		d.Keywords = splitZero(kw)
		d.Triggers = splitZero(trg)
		d.Pinned = pinned != 0
		d.CreatedAt = time.UnixMilli(createdMS).UTC()
		out = append(out, matchResult{doc: d, rel: bm25Weight(rank)})
	}
	return out, rows.Err()
}

func (b *ftsBackend) close() error {
	return b.db.Close()
}

// docKey is the row identity of one doc: its file path, or a synthetic key
// for callers that add a memory without one.
func docKey(d indexDoc) string {
	if d.Path != "" {
		return d.Path
	}
	return "id:" + d.Domain + ":" + d.ID
}

// joinTokens builds the indexed vocabulary of one document. Tokens are NOT
// deduplicated: their frequency feeds bm25.
func joinTokens(d indexDoc) string {
	return strings.Join(indexTokens(docText(d)), " ")
}

// quoteFTS makes one token a literal fts5 string (tokens never contain
// quotes; escaping keeps the query injection-proof).
func quoteFTS(tok string) string {
	return `"` + strings.ReplaceAll(tok, `"`, `""`) + `"`
}

// bm25Weight converts the bm25() rank (lower = better, 0 = best, <= 0 in
// practice) into a positive relevance weight comparable across queries.
func bm25Weight(rank float64) float64 {
	if rank > 0 {
		rank = 0
	}
	return 1 - rank
}

// discardForeignCache removes a cache file written by the other backend so
// SQLite does not trip over a JSON file and vice versa.
func discardForeignCache(path string, wantSQLite bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	isSQLite := len(data) >= len(sqliteMagic) && bytes.Equal(data[:len(sqliteMagic)], sqliteMagic)
	if isSQLite == wantSQLite {
		return nil
	}
	return os.Remove(path)
}

func inTx(db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func lastRowID(tx *sql.Tx) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT last_insert_rowid()`).Scan(&id)
	return id, err
}

func splitZero(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}
