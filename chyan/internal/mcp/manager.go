package mcp

import (
	"context"
	"fmt"

	"chyan.tech/chyan/internal/core"
)

// Manager owns the configured MCP servers and their tool adapters
// (architecture 4.8: connect + catalog at startup, per-server degradation).
type Manager struct {
	servers []*Server
	opts    ToolOptions
	// tools counts the adapters registered per server. Exact attribution: the
	// status panel must not guess from tool-name prefixes (two server names can
	// share a prefix).
	tools map[string]int
}

// New builds a manager from parsed server configs. Disabled or invalid
// entries are skipped (an unusable config must not block other servers).
func New(cfgs []ServerConfig, secrets Secrets, opts ToolOptions) *Manager {
	m := &Manager{opts: opts}
	for _, cfg := range cfgs {
		s := newServer(cfg, secrets)
		if !s.Enabled() {
			continue
		}
		m.servers = append(m.servers, s)
	}
	return m
}

// Connect connects every server and fetches its tool catalog into reg.
// One server's failure degrades only that server (logged, tools skipped);
// the first registration conflict is reported but does not stop others.
func (m *Manager) Connect(ctx context.Context, reg *core.ToolRegistry) error {
	var firstErr error
	m.tools = make(map[string]int, len(m.servers))
	for _, s := range m.servers {
		tools, err := s.listTools(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, mt := range tools {
			if err := reg.Register(newTool(s.Name(), mt, s, m.opts)); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("mcp %s: %w", s.Name(), err)
				}
				continue
			}
			m.tools[s.Name()]++
		}
	}
	return firstErr
}

// ToolCounts reports how many adapters each server registered (observability).
func (m *Manager) ToolCounts() map[string]int {
	out := make(map[string]int, len(m.tools))
	for name, n := range m.tools {
		out[name] = n
	}
	return out
}

// Available reports the live-connection state per server (observability).
func (m *Manager) Available() map[string]bool {
	out := make(map[string]bool, len(m.servers))
	for _, s := range m.servers {
		out[s.Name()] = s.Available()
	}
	return out
}

// Close shuts down all server connections (kills local child processes).
func (m *Manager) Close() {
	for _, s := range m.servers {
		_ = s.Close()
	}
}
