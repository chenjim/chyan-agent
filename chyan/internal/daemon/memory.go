// Memory management handlers: the Web memory view lists/searches long-term
// memories, marks a wrong memory invalid (supersede) and views/runs the
// dream maintenance pass (FR-12 W1).
package daemon

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/memory"
	"chyan.tech/chyan/internal/store"
)

// handleListMemory lists or searches memories (GET /api/v1/memory).
// Query: q (keywords), domain (global|project), kind (comma list), project.
func (s *Server) handleListMemory(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	domain := r.URL.Query().Get("domain")
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if project != "" {
		resolved, err := app.ResolveWorkDir(s.app.WorkDir(), project)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		project = resolved
	}
	kinds := splitKinds(r.URL.Query().Get("kind"))

	var hits []store.Memory
	var err error
	if q != "" {
		hits, err = s.app.Memory().SearchMemories(r.Context(), memory.Query{
			Text: q, Kinds: kinds, ProjectPath: project,
		})
	} else {
		hits, err = s.app.Memory().ListMemories(r.Context(), project, kinds)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	entries := make([]api.MemoryEntry, 0, len(hits))
	for _, m := range hits {
		if domain != "" && domain != "all" && m.Domain != domain {
			continue
		}
		entries = append(entries, memoryEntry(m))
	}
	writeJSON(w, http.StatusOK, entries)
}

// memoryEntry maps one store memory to the API shape.
func memoryEntry(m store.Memory) api.MemoryEntry {
	return api.MemoryEntry{
		ID: m.ID, Kind: m.Kind, Domain: m.Domain, ProjectPath: m.ProjectPath,
		Name: m.Name, Keywords: m.Keywords, Triggers: m.Triggers,
		Content: m.Content, Status: m.Status, Path: m.Path, CreatedAt: m.CreatedAt,
	}
}

// splitKinds parses "fact,kb" into ["fact","kb"].
func splitKinds(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, k := range strings.Split(raw, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// handleSupersedeMemory marks one memory invalid
// (POST /api/v1/memory/{id}/supersede?project=<repo>). The file is kept as
// history; recall and search stop returning it.
func (s *Server) handleSupersedeMemory(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if project != "" {
		resolved, err := app.ResolveWorkDir(s.app.WorkDir(), project)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		project = resolved
	}
	m, err := s.app.Memory().Supersede(r.Context(), r.PathValue("id"), project)
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, "memory not found")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, memoryEntry(m))
}

// handleGetDream returns the latest dream report (GET /api/v1/memory/dream).
func (s *Server) handleGetDream(w http.ResponseWriter, _ *http.Request) {
	rep, ok := s.app.LastDreamReport()
	if !ok {
		writeError(w, http.StatusNotFound, "no dream report yet")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleRunDream triggers one dream pass and returns its report
// (POST /api/v1/memory/dream).
func (s *Server) handleRunDream(w http.ResponseWriter, r *http.Request) {
	rep, err := s.app.RunDreamAndRecord(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
