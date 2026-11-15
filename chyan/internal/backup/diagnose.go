package backup

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// secretKeyRe matches config keys whose values must be redacted in the
// diagnostics bundle (D20: credentials live in secret/, but config may
// still carry inline values).
var secretKeyRe = regexp.MustCompile(`(?i)(key|token|secret|password|passwd|credential)`)

// inlineSecretRe masks secret-looking assignments inside inline tables and
// arrays (headers = { api_key = "..." }), which the line-level key test misses.
var inlineSecretRe = regexp.MustCompile(
	`(?i)([a-z0-9_.-]*(?:key|token|secret|password|passwd|credential)[a-z0-9_.-]*)(\s*=\s*)("[^"]*"|'[^']*'|[^,}\s]+)`)

// secretValueRe masks values that are self-evident credentials even when the
// key name is innocuous (headers = { Authorization = "Bearer sk-..." }).
var secretValueRe = regexp.MustCompile(
	`(?i)(sk-[a-z0-9_-]{8,}|bearer [a-z0-9._~+/=-]{8,}|ghp_[a-z0-9]{20,}|github_pat_[a-z0-9_]{20,}|xox[abp]-[a-z0-9-]{8,}|akia[a-z0-9]{12,}|aiza[a-z0-9_-]{20,})`)

// DiagInfo is the caller-supplied summary that does not live on disk.
type DiagInfo struct {
	Version string
	OS      string
	Arch    string
}

// Diagnose writes a gzipped diagnostics tar to w. Everything is redacted or
// summarized: config values under secret-looking keys become "***",
// jobs.json is reduced to counts, chyan.db to table/row counts, and the
// last audit entries to action/verdict/approved only. The secret/ directory
// is never packed.
func Diagnose(ctx context.Context, home string, w io.Writer, info DiagInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	summary := map[string]any{
		"version": info.Version,
		"os":      info.OS,
		"arch":    info.Arch,
		"time":    time.Now().UTC().Format(time.RFC3339),
		"home":    home,
	}

	if raw, err := os.ReadFile(filepath.Join(home, "config.toml")); err == nil {
		summary["config"] = string(redactConfig(raw))
	} else if !os.IsNotExist(err) {
		return err
	}
	if raw, err := os.ReadFile(filepath.Join(home, "jobs.json")); err == nil {
		summary["jobs"] = jobsSummary(raw)
	}
	if dbSummary, err := dbSummary(filepath.Join(home, dbFileName)); err == nil {
		summary["db"] = dbSummary
	} else if !os.IsNotExist(err) {
		summary["db_error"] = err.Error()
	}
	if audit, err := auditSummary(filepath.Join(home, dbFileName)); err == nil {
		summary["audit_recent"] = audit
	}

	body, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	hdr := &tar.Header{
		Name: "diagnose.json", Typeflag: tar.TypeReg,
		Mode: 0o600, Size: int64(len(body)), ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = tw.Write(body)
	return err
}

// redactConfig masks the values of secret-looking TOML keys in place.
func redactConfig(raw []byte) []byte {
	var out strings.Builder
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if eq := strings.Index(line, "="); eq >= 0 {
			key := strings.TrimSpace(line[:eq])
			if secretKeyRe.MatchString(key) {
				out.WriteString(line[:eq])
				out.WriteString(" = \"***\"\n")
				continue
			}
			// Inline tables/arrays: redact secret-looking assignments and
			// credential-shaped values without losing the surrounding config.
			line = inlineSecretRe.ReplaceAllString(line, `${1}${2}"***"`)
		}
		line = secretValueRe.ReplaceAllString(line, `"***"`)
		out.WriteString(line)
		out.WriteString("\n")
	}
	return []byte(out.String())
}

// jobsSummary reduces the job list to counts (prompts may embed secrets).
func jobsSummary(raw []byte) map[string]any {
	var jobs []struct {
		Enabled bool `json:"enabled"`
	}
	summary := map[string]any{"parse_error": false}
	if err := json.Unmarshal(raw, &jobs); err != nil {
		summary["parse_error"] = true
		return summary
	}
	enabled := 0
	for _, j := range jobs {
		if j.Enabled {
			enabled++
		}
	}
	summary["total"] = len(jobs)
	summary["enabled"] = enabled
	return summary
}

// dbSummary reports table names and row counts.
func dbSummary(dbPath string) (map[string]any, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := map[string]int64{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		var n int64
		if err := db.QueryRow("SELECT COUNT(*) FROM " + quoteIdent(name)).Scan(&n); err == nil {
			tables[name] = n
		}
	}
	return map[string]any{"tables": tables}, rows.Err()
}

// auditSummary lists the most recent audit rows in summary form only.
func auditSummary(dbPath string) ([]map[string]any, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT action, verdict, approved FROM audit ORDER BY id DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var action, verdict string
		var approved bool
		if err := rows.Scan(&action, &verdict, &approved); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"action": action, "verdict": verdict, "approved": approved})
	}
	return out, rows.Err()
}

// quoteIdent guards the table name interpolated above (names come from
// sqlite_master, so this is belt-and-braces).
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
