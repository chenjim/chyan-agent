// Workspace selection for daemon tasks (FR-12 W1). A task may run against any
// existing directory on the daemon machine; a continued session reuses the
// workspace it was created in unless the request explicitly overrides it.
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
)

const (
	// workspaceRecentKey stores the most recent workspace paths.
	workspaceRecentKey = "workspace:recent"
	// workspaceRecentLimit bounds the client-visible history.
	workspaceRecentLimit = 10
)

func workspaceSessionKey(sessionID string) string { return "workspace:session:" + sessionID }

// workspaceInfo reports the daemon default plus recent selections.
func (s *Server) workspaceInfo(ctx context.Context) api.WorkspaceInfo {
	info := api.WorkspaceInfo{Path: s.app.WorkDir(), Artifacts: s.app.ArtifactRoot()}
	raw, err := s.store.GetKV(ctx, workspaceRecentKey)
	if err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &info.Recent)
	}
	// The default is always offered first and never duplicated.
	out := make([]string, 0, workspaceRecentLimit)
	out = append(out, info.Path)
	for _, p := range info.Recent {
		if p != info.Path && len(out) < workspaceRecentLimit {
			out = append(out, p)
		}
	}
	info.Recent = out
	return info
}

// handleWorkspace returns the default and recent workspaces (GET /api/v1/workspace).
func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.workspaceInfo(r.Context()))
}

// taskWorkDir resolves the workspace for one task. An explicit request wins;
// otherwise a continued session keeps its recorded workspace; otherwise the
// daemon default applies. The mapping is persisted so later turns stay in the
// same project.
func (s *Server) taskWorkDir(ctx context.Context, req api.CreateTaskRequest) (string, error) {
	base := s.app.WorkDir()
	dir, err := app.ResolveWorkDir(base, req.WorkDir)
	if err != nil {
		return "", err
	}
	explicit := req.WorkDir != ""
	if !explicit && req.SessionID != "" {
		if raw, err := s.store.GetKV(ctx, workspaceSessionKey(req.SessionID)); err == nil && len(raw) > 0 {
			// A recorded path may have been deleted since: fall back to the
			// default rather than failing a session that used to work.
			if resolved, rerr := app.ResolveWorkDir(base, string(raw)); rerr == nil {
				dir = resolved
			}
		}
	}
	if req.SessionID != "" {
		_ = s.store.SetKV(ctx, workspaceSessionKey(req.SessionID), []byte(dir))
	}
	if dir != base {
		s.rememberWorkspace(ctx, dir)
	}
	return dir, nil
}

// rememberWorkspace prepends a path to the recent list (deduped, bounded).
func (s *Server) rememberWorkspace(ctx context.Context, dir string) {
	raw, _ := s.store.GetKV(ctx, workspaceRecentKey)
	var recent []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &recent)
	}
	out := []string{dir}
	for _, p := range recent {
		if p != dir && len(out) < workspaceRecentLimit {
			out = append(out, p)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	_ = s.store.SetKV(ctx, workspaceRecentKey, b)
}

// workspaceRoot resolves the file viewer's root: workspace (default) or the
// artifact spill directory. Both stay sandboxed to their own root.
func (s *Server) workspaceRoot(r *http.Request) (string, error) {
	if r.URL.Query().Get("source") == "artifacts" {
		root := s.app.ArtifactRoot()
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", err
		}
		return root, nil
	}
	return app.ResolveWorkDir(s.app.WorkDir(), r.URL.Query().Get("workspace"))
}
