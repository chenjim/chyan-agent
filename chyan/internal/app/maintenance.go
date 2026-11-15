// Startup/nightly maintenance: cache TTL cleanup (architecture 4.6, three
// trigger points) and the dream memory pass (architecture 4.3).
package app

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"chyan.tech/chyan/internal/memory"
)

// PurgeCache removes cache artifacts older than the configured TTL
// (cache_ttl_hours) and prunes empty directories. The cache only holds
// intermediate artifacts, so deletion is safe by design.
func (a *App) PurgeCache() error {
	ttl := time.Duration(a.cfg.Cache.TTLHours) * time.Hour
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return cleanCache(filepath.Join(a.home, "cache"), time.Now(), ttl)
}

// cleanCache walks root deleting files older than ttl and empty dirs left
// behind (root itself is kept).
func cleanCache(root string, now time.Time, ttl time.Duration) error {
	cutoff := now.Add(-ttl)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // race with cleanup: treat as gone
		}
		if info.ModTime().Before(cutoff) {
			return os.Remove(path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return pruneEmptyDirs(root)
}

// pruneEmptyDirs removes empty directories under root bottom-up (root kept).
func pruneEmptyDirs(root string) error {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		entries, err := os.ReadDir(dirs[i])
		if err == nil && len(entries) == 0 {
			_ = os.Remove(dirs[i])
		}
	}
	return nil
}

// RunDream executes one dream cycle over the memory root (dedupe/supersede
// chains, project attribution notes, orphan counting) and returns the
// rollbackable report (architecture 4.3). The latest report is kept on disk
// so the Web can show it after a restart.
func (a *App) RunDream(ctx context.Context) (memory.DreamReport, error) {
	a.dreamMu.Lock()
	defer a.dreamMu.Unlock()
	root := a.memoryRoot()
	d := memory.NewDreamer(memory.DreamConfig{Root: root, Index: a.memIndex})
	rep, err := d.Run(ctx)
	if err != nil || a.memIndex == nil {
		return rep, err
	}
	// Rebuild after the pass so files changed outside the dreamer (e.g. a
	// crash mid-cycle) also land in the cache; the md files stay the truth.
	if rerr := a.memIndex.Rebuild(root); rerr != nil {
		log.Printf("app: memory index rebuild degraded: %v", rerr)
	}
	return rep, nil
}

// RunDreamAndRecord runs one dream pass and remembers the report for the
// management API (daemon). Errors are returned; the last good report stays.
func (a *App) RunDreamAndRecord(ctx context.Context) (memory.DreamReport, error) {
	rep, err := a.RunDream(ctx)
	if err != nil {
		return rep, err
	}
	a.dreamMu.Lock()
	a.lastDream = &rep
	a.dreamMu.Unlock()
	if werr := a.writeDreamReport(rep); werr != nil {
		log.Printf("app: dream report persist degraded: %v", werr)
	}
	return rep, nil
}

// LastDreamReport returns the most recent dream report: the in-memory copy
// when this process ran one, else the persisted file. ok is false when no
// pass has run yet.
func (a *App) LastDreamReport() (memory.DreamReport, bool) {
	a.dreamMu.Lock()
	rep := a.lastDream
	a.dreamMu.Unlock()
	if rep != nil {
		return *rep, true
	}
	path := filepath.Join(a.home, "state", "dream-report.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return memory.DreamReport{}, false
	}
	var out memory.DreamReport
	if json.Unmarshal(data, &out) != nil {
		return memory.DreamReport{}, false
	}
	return out, true
}

// writeDreamReport persists the report atomically (0600).
func (a *App) writeDreamReport(rep memory.DreamReport) error {
	dir := filepath.Join(a.home, "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "dream-report.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// memoryRoot is the root the FileStore was built with (dream target).
func (a *App) memoryRoot() string {
	return a.memRoot
}

// runStartupMaintenance does the lazy startup cache clean (architecture 4.6
// primary trigger); the dream pass runs on the daemon's own cadence via
// DreamOnce, not on every CLI invocation.
func (a *App) runStartupMaintenance() {
	if err := a.PurgeCache(); err != nil {
		log.Printf("app: cache purge degraded: %v", err)
	}
}

// DreamOnce runs one dream cycle and logs its summary (daemon startup acts
// as the nightly trigger; cron can call it too).
func (a *App) DreamOnce(ctx context.Context) {
	rep, err := a.RunDreamAndRecord(ctx)
	if err != nil {
		log.Printf("app: dream pass degraded: %v", err)
		return
	}
	log.Printf("app: dream pass: examined=%d merged=%d orphaned=%d notes=%d",
		rep.Examined, rep.Merged, rep.Orphaned, len(rep.Notes))
}
