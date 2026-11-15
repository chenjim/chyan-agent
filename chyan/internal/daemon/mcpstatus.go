// mcpstatus.go — MCP server observability for GET /api/v1/status (U2).
// The status bar shows "mcp ok/total" and the TUI palette expands the detail,
// so a server that failed to connect is visible instead of silently missing
// its tools.
package daemon

import (
	"sort"

	"chyan.tech/chyan/internal/api"
)

// mcpStatuses joins the live connection map with the adapters each server
// registered (counts come from the manager, not from name prefixes: two server
// names can share a prefix). Servers are sorted by name so the panel order is
// stable across polls.
func mcpStatuses(available map[string]bool, tools map[string]int) []api.MCPStatus {
	if len(available) == 0 {
		return nil
	}
	out := make([]api.MCPStatus, 0, len(available))
	for name, ok := range available {
		out = append(out, api.MCPStatus{Name: name, Available: ok, Tools: tools[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
