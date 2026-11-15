// configreload.go — POST /api/v1/config/reload: re-read config.toml in place
// (U3, daemon side). The app refuses while a turn is in flight, so the handler
// maps that to 409 and the client retries when the session goes idle.
package daemon

import (
	"errors"
	"net/http"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
)

// handleReloadConfig reloads the daemon's configuration and reports what was
// applied. Serialized by reloadMu: two concurrent reloads would race on the
// MCP reconnect even though the app itself is consistent.
func (s *Server) handleReloadConfig(w http.ResponseWriter, _ *http.Request) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	res, err := s.app.Reload()
	if errors.Is(err, app.ErrBusy) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.ReloadResult{
		ConfigPath: res.ConfigPath,
		Model:      res.Model,
		ReviewMode: res.ReviewMode,
		PermRules:  res.PermRules,
		Skills:     res.Skills,
		Commands:   res.Commands,
		MCPServers: res.MCPServers,
		Tools:      res.Tools,
	})
}
