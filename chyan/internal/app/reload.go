// reload.go — hot configuration reload (REPL /reload, U3). Everything that
// can change without restarting the process is re-read from config.toml;
// a config that cannot be applied leaves the running setup untouched.
package app

import (
	"errors"
	"log"

	"chyan.tech/chyan/internal/config"
	"chyan.tech/chyan/internal/core"
)

// ReloadResult reports what one reload re-read, so the caller can show a
// one-line confirmation instead of asking the user to re-open the file.
type ReloadResult struct {
	ConfigPath string
	Model      string
	ReviewMode string
	PermRules  int
	Skills     int
	Commands   int
	MCPServers int
	Tools      int
}

// ErrBusy is returned by Reload when a turn is in flight: the caller should
// retry between turns instead of waiting for a long run to finish.
var ErrBusy = errors.New("app: a run is in flight; retry the reload after it finishes")

// Reload re-reads config.toml and re-applies the reloadable subset: LLM
// routing (skipped when the caller injected a backend), context windows,
// review mode, permission rules, tool limits, MCP servers, skills/commands and
// the AGENTS.md directive block.
//
// All-or-nothing: parse errors, an unusable [llm] endpoint or an unbuildable
// gate return an error before anything is swapped, so the live configuration
// keeps working. It refuses (ErrBusy) instead of waiting while a turn runs:
// runs hold runMu for their whole lifetime, and the swap takes the write side
// so a turn can never observe a half-swapped config.
func (a *App) Reload() (ReloadResult, error) {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()

	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		return ReloadResult{}, err
	}
	backend := a.backend
	if !a.backendInjected {
		if backend, err = buildBackend(cfg, a.secrets); err != nil {
			return ReloadResult{}, err
		}
	}
	var gate ToolGate
	if !a.opts.SkipGate {
		opts := a.opts
		opts.Backend = backend
		if gate, err = buildGate(cfg, opts); err != nil {
			return ReloadResult{}, err
		}
	}
	// Build the replacement tool set before touching anything: tools capture
	// their limits at construction (output cap, bash timeout), so a failed
	// registration must not leave the app with a half-built registry.
	toolSet, rawTools, err := a.buildTools(cfg)
	if err != nil {
		return ReloadResult{}, err
	}

	// Everything validated. Refuse rather than wait when a turn is running, then
	// swap behind both locks so readers (status accessors) and runs never see a
	// mixed configuration.
	if !a.runMu.TryLock() {
		return ReloadResult{}, ErrBusy
	}
	defer a.runMu.Unlock()
	a.wireMu.Lock()
	a.cfg = cfg
	a.backend = backend
	a.windows = windowConfig(cfg)
	a.reviewMode = reviewMode(cfg, a.opts.Auto)
	a.tools, a.rawTools = toolSet, rawTools
	core.SetToolOutputMaxBytes(cfg.Tools.ToolOutputMaxBytes)
	if gate != nil {
		a.SetGate(gate)
		a.permChecker = buildChecker(cfg)
	}
	// MCP reconnects after the swap: one server's failure degrades only that
	// server, exactly like startup.
	a.reconnectMCP()
	a.loadExtensions(a.opts)
	res := ReloadResult{
		ConfigPath: a.cfgPath,
		Model:      cfg.LLM.Model,
		ReviewMode: string(a.reviewMode),
		PermRules:  len(cfg.Permission),
		Skills:     len(a.skills),
		Commands:   len(a.commands),
		MCPServers: a.mcpServerCount(),
		Tools:      len(a.tools.Tools()),
	}
	a.wireMu.Unlock()
	return res, nil
}

// reconnectMCP drops the old MCP clients and connects the configured servers
// into the current registry.
func (a *App) reconnectMCP() {
	if a.mcp != nil {
		a.mcp.Close()
		a.mcp = nil
	}
	if err := a.connectMCP(); err != nil {
		log.Printf("app: mcp connect degraded: %v", err)
	}
}

// mcpServerCount reports how many MCP servers are configured (the reload
// confirmation line shows it so a dropped server is visible).
func (a *App) mcpServerCount() int {
	n := 0
	for _, sc := range a.cfg.MCP.Servers {
		if !sc.Disabled {
			n++
		}
	}
	return n
}
