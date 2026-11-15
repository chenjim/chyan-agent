// sessions_extra.go — session-adjacent endpoints the CLI needs once it becomes
// a pure daemon client: create an empty session, compact, record a shell note
// and list project commands. Each call reuses the same app methods as the
// in-process CLI did, so behavior does not fork.
package daemon

import (
	"errors"
	"net/http"
	"strings"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/store"
)

// handleCreateSession starts an empty conversation (POST /api/v1/sessions).
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req api.CreateSessionRequest
	if err := decodeBodyLimit(w, r, &req, 64<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sid, err := s.app.EnsureSession(r.Context(), "", req.Agent, req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	se, err := s.store.GetSession(r.Context(), sid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sessionSummary(se))
}

// handleCompactSession folds one session's history into a handoff note
// (POST /api/v1/sessions/{id}/compact). The id may be "continue".
func (s *Server) handleCompactSession(w http.ResponseWriter, r *http.Request) {
	note, err := s.app.Compact(r.Context(), r.PathValue("id"))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.CompactResult{Note: note})
}

// handleAppendNote records a user-run shell command and its output as a user
// message (POST /api/v1/sessions/{id}/note). No LLM call and no approval apply:
// the human typed it.
func (s *Server) handleAppendNote(w http.ResponseWriter, r *http.Request) {
	var req api.AppendNoteRequest
	if err := decodeBodyLimit(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}
	sid, err := s.app.AppendShellNote(r.Context(), r.PathValue("id"), req.Command, req.Output)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.AppendNoteResult{SessionID: sid})
}

// handleListCommands lists project slash commands (GET /api/v1/commands) so a
// remote CLI can render help/completion without touching the filesystem.
func (s *Server) handleListCommands(w http.ResponseWriter, _ *http.Request) {
	cmds := s.app.Commands()
	out := make([]api.CommandInfo, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, api.CommandInfo{Name: c.Name, Description: c.Description})
	}
	writeJSON(w, http.StatusOK, out)
}
