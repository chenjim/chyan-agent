// prune.go — retention for sessions, audit rows and artifact files
// (FR-11/D34). Dry-run is the default; --yes performs the deletion.
package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"chyan.tech/chyan/internal/store"
)

func pruneCmd() *cobra.Command {
	var (
		home      string
		kind      string
		olderThan time.Duration
		yes       bool
	)
	cmd := &cobra.Command{
		Use:   "prune --kind session|audit|trace|artifacts --older-than 720h [--yes]",
		Short: "Delete data older than a retention window (dry-run unless --yes)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if olderThan <= 0 {
				return fmt.Errorf("prune: --older-than must be positive")
			}
			h, err := homeFlag(home)
			if err != nil {
				return err
			}
			cutoff := time.Now().Add(-olderThan)
			switch kind {
			case "artifacts":
				files, bytes, err := pruneArtifacts(filepath.Join(h, "artifacts"), cutoff, !yes)
				if err != nil {
					return err
				}
				fmt.Printf("prune artifacts：older-than %s（before %s），命中 %d 个文件 / %s\n",
					olderThan, cutoff.Format(time.RFC3339), files, humanBytes(bytes))
			case "session", "audit", "trace":
				st, err := openDataStore(h)
				if err != nil {
					return err
				}
				defer st.Close()
				var n int
				switch kind {
				case "session":
					n, err = st.PruneSessions(cmd.Context(), cutoff, !yes)
				case "audit":
					n, err = st.PruneAudit(cmd.Context(), cutoff, !yes)
				default:
					n, err = pruneTraceSpans(cmd.Context(), st, cutoff, !yes)
				}
				if err != nil {
					return err
				}
				fmt.Printf("prune %s：older-than %s（before %s），命中 %d 条\n",
					kind, olderThan, cutoff.Format(time.RFC3339), n)
			default:
				return fmt.Errorf("prune: --kind must be session|audit|trace|artifacts, got %q", kind)
			}
			if !yes {
				fmt.Println("（dry-run，未删除；加 --yes 执行）")
			} else {
				fmt.Println("已执行。")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&home, "home", "", "chyan data root (default ~/.chyan)")
	cmd.Flags().StringVar(&kind, "kind", "", "what to prune: session|audit|artifacts")
	cmd.Flags().DurationVar(&olderThan, "older-than", 720*time.Hour, "retention window (older entries are pruned)")
	cmd.Flags().BoolVar(&yes, "yes", false, "actually delete (default: dry-run)")
	return cmd
}

// spanPruner is the trace retention capability the prune command needs; it
// stays off store.Store so in-memory test doubles do not grow an unused method.
type spanPruner interface {
	PruneSpans(ctx context.Context, before time.Time, dryRun bool) (int, error)
}

// pruneTraceSpans deletes trace metadata older than before. Traces are
// diagnostic data: a missing capability is an error, never a silent no-op.
func pruneTraceSpans(ctx context.Context, st store.Store, before time.Time, dryRun bool) (int, error) {
	p, ok := st.(spanPruner)
	if !ok {
		return 0, fmt.Errorf("prune: this store does not support trace spans")
	}
	return p.PruneSpans(ctx, before, dryRun)
}

// openDataStore opens <home>/chyan.db without wiring the LLM/gate stack:
// maintenance commands must work on a machine with no usable model config.
func openDataStore(home string) (store.Store, error) {
	path := filepath.Join(home, "chyan.db")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no database at %s (run chyan once first)", path)
		}
		return nil, err
	}
	return store.NewSQLite(path)
}

func vacuumCmd() *cobra.Command {
	var home string
	cmd := &cobra.Command{
		Use:   "vacuum",
		Short: "Compact the database and rebuild indexes/statistics",
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := homeFlag(home)
			if err != nil {
				return err
			}
			st, err := openDataStore(h)
			if err != nil {
				return err
			}
			defer st.Close()
			m, ok := st.(interface {
				Maintain(context.Context) error
			})
			if !ok {
				return fmt.Errorf("vacuum: this store does not support maintenance")
			}
			if err := m.Maintain(cmd.Context()); err != nil {
				return err
			}
			fmt.Println("数据库已压缩并重建索引。")
			return nil
		},
	}
	cmd.Flags().StringVar(&home, "home", "", "chyan data root (default ~/.chyan)")
	return cmd
}

// pruneArtifacts removes regular files under root older than before and then
// prunes directories left empty. Symlinks are never followed or removed.
func pruneArtifacts(root string, before time.Time, dryRun bool) (files int, bytes int64, err error) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return 0, 0, nil
	}
	var dirs []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.ModTime().Before(before) {
			return nil
		}
		files++
		bytes += info.Size()
		if dryRun {
			return nil
		}
		return os.Remove(path)
	})
	if err != nil || dryRun {
		return files, bytes, err
	}
	// Children first: a directory can only be removed once its files are gone.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		if dir == root {
			continue
		}
		_ = os.Remove(dir) // fails (and is ignored) when non-empty
	}
	return files, bytes, nil
}

// dirSize sums regular file sizes below root (missing root = 0).
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// fileSize reports one file's size (missing file = 0).
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// humanBytes formats a byte count with one decimal for the MB/GB bands.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(n)
	for _, u := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f PB", value/unit)
}
