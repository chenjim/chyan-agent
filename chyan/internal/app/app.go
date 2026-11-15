// Package app assembles a runnable chyan agent from configuration: config +
// secrets + memory + tools + LLM routing + core loop. cmd/chyan and the P3
// daemon both drive this layer.
package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"chyan.tech/chyan/internal/channels"
	"chyan.tech/chyan/internal/commands"
	"chyan.tech/chyan/internal/config"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/deliver"
	"chyan.tech/chyan/internal/frontmatter"
	"chyan.tech/chyan/internal/instructions"
	"chyan.tech/chyan/internal/llm"
	"chyan.tech/chyan/internal/mcp"
	"chyan.tech/chyan/internal/memory"
	"chyan.tech/chyan/internal/secret"
	"chyan.tech/chyan/internal/skills"
	"chyan.tech/chyan/internal/store"
	"chyan.tech/chyan/internal/tools"
	"chyan.tech/chyan/internal/trace"

	"gopkg.in/yaml.v3"
)

// Options configure assembly. Zero values pick production defaults.
type Options struct {
	// WorkDir is the tool sandbox root (default: cwd). Locked per process.
	WorkDir string
	// Home is the chyan data root (default: ~/.chyan). CHYAN_HOME overrides.
	Home string
	// ConfigPath defaults to <Home>/config.toml.
	ConfigPath string
	// MemoryRoot is the global .agents/memory root (tests inject a temp dir).
	MemoryRoot string
	// Backend overrides the LLM built from config (tests inject a fake).
	Backend core.LLMBackend
	// Store overrides the persistence layer (tests inject in-memory).
	Store store.Store
	// ExtensionsDir overrides the project .agents/ root for skills and
	// commands. Tests point it at a temp dir to avoid scanning the real
	// ~/.agents (which has 123 skills and is slow on every New).
	ExtensionsDir string
	// SkipExtensions disables skills/commands loading entirely (tests).
	SkipExtensions bool
	// Channels overrides the channel registry built from config (tests
	// inject a fake; nil builds from [channels.<name>] as usual).
	Channels *channels.Registry
	// Reviewer is the LLM risk reviewer injected by callers/tests. When nil,
	// one is built from the config [review].model setting (empty model keeps
	// the rules/heuristic-only gate).
	Reviewer core.RiskReviewer
	// Approver is the human approval channel (CLI/TUI/PWA/IM); nil means no
	// human is reachable and escalated asks conservatively deny (D24).
	Approver core.Approver
	// Auto forces review mode "full" (--auto): ask-effect requests
	// auto-approve except explicit deny rules.
	Auto bool
	// SkipGate disables gate assembly entirely (P2 pass-through; tests).
	SkipGate bool
	// Tracer overrides span recording (tests inject an in-memory sink). When
	// nil, the persistence layer is used if it implements trace.Sink.
	Tracer *trace.Recorder
}

// App is one wired agent instance.
type App struct {
	cfg     config.Config
	workDir string
	home    string
	memRoot string // memory truth-source root (dream target)

	store  store.Store
	memory *memory.FileStore
	// memIndex is the derived full-text cache over the memory truth source
	// (architecture 4.3). Nil when the cache could not be opened: lookups then
	// scan the markdown files directly.
	memIndex *memory.Index
	secrets  *secret.Store
	backend  core.LLMBackend
	windows  *llm.WindowConfig // layered window resolution (FR-1)
	tools    *core.ToolRegistry
	mcp      *mcp.Manager

	channels *channels.Registry
	deliver  *deliver.Router

	sessMu         sync.Mutex // guards currentSession (concurrent runs)
	currentSession string
	agentMu        sync.RWMutex           // guards the per-agent knobs below
	prompt         string                 // system prompt override (SetAgent)
	modelOverride  string                 // per-run model override (--model)
	agentModel     string                 // model from the agent definition frontmatter
	agentPerms     map[string]core.Effect // agent-level permission subset (FR-5)
	// reviewMode is the effective decision mode (--auto forces "full"); status
	// surfaces report it without re-parsing options.
	reviewMode core.ReviewMode
	gate       ToolGate
	// permChecker holds the config permission rules. The review gate consults
	// it for ask-path tools; read-only tools (which skip the gate) consult it
	// directly so an explicit deny can never be bypassed.
	permChecker *core.PermSet
	skills      []skills.Skill
	commands    []commands.Command
	// directives is the rendered AGENTS.md/CLAUDE.md/.cursor-rules block
	// appended to every system prompt (empty when disabled or absent).
	directives string
	rawTools   map[string]core.Tool // unwrapped tools, re-wrapped per run

	subOnce   sync.Once
	subRunner *core.SubAgentRunner

	// dreamMu serializes dream passes and guards lastDream (startup pass and
	// a manual Web trigger may overlap).
	dreamMu   sync.Mutex
	lastDream *memory.DreamReport

	tracer *trace.Recorder // local span recorder (FR-13), never nil

	// reload wiring (/reload): the config path, the assembly options that
	// built the gate, and the guard that serializes reloads (U3).
	cfgPath string
	opts    Options
	// backendInjected marks a caller-supplied LLM backend (tests): reload then
	// keeps it instead of rebuilding from [llm].
	backendInjected bool
	reloadMu        sync.Mutex
	// runMu is held for reading by every in-flight turn and taken for writing by
	// Reload (TryLock: reload never waits for a long turn, it refuses instead).
	runMu sync.RWMutex
	// wireMu guards the reload-swappable wiring (cfg/tools/mcp/backend/review
	// mode) for accessors called from other goroutines (daemon status). Runs do
	// not need it: runMu keeps them out of a swap.
	wireMu sync.RWMutex
}

// loadExtensions reads skills and slash commands from the global
// ~/.agents/roots and the project .agents/ roots (architecture 4.4/4.9).
// Broken entries are skipped by the loaders; missing roots are fine.
// opts.ExtensionsDir / opts.SkipExtensions let tests avoid the real ~/.agents.
func (a *App) loadExtensions(opts Options) {
	if opts.SkipExtensions {
		return
	}
	var globalSkills, globalCmds string
	if opts.ExtensionsDir == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			globalSkills = filepath.Join(userHome, ".agents", "skills")
			globalCmds = filepath.Join(userHome, ".agents", "commands")
		}
	}
	projectRoot := filepath.Join(a.workDir, ".agents")
	if opts.ExtensionsDir != "" {
		projectRoot = opts.ExtensionsDir
	}
	projectSkills := filepath.Join(projectRoot, "skills")
	projectCmds := filepath.Join(projectRoot, "commands")

	if sks, err := skills.Load(globalSkills, projectSkills); err == nil {
		a.skills = sks
	}
	if cmds, err := commands.Load(globalCmds, projectCmds); err == nil {
		a.commands = cmds
	}
	if a.cfg.Context.LoadProjectInstructions {
		base := ""
		if opts.ExtensionsDir == "" {
			if userHome, err := os.UserHomeDir(); err == nil {
				base = filepath.Join(userHome, ".agents")
			}
		}
		a.directives = instructions.Render(instructions.Load(base, a.workDir))
	}
}

// New wires the full stack. It creates ~/.chyan, loads config and secrets,
// registers built-in and MCP tools, and opens the session store.
func New(opts Options) (*App, error) {
	home := opts.Home
	if home == "" {
		var err error
		home, err = config.Home()
		if err != nil {
			return nil, fmt.Errorf("app: resolve home: %w", err)
		}
	}
	_, notice, err := config.InitDirs()
	if err != nil {
		return nil, fmt.Errorf("app: init dirs: %w", err)
	}
	_ = notice // one-shot migration notice surfaced by callers via LogNotice

	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		cfgPath = filepath.Join(home, "config.toml")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("app: load config: %w", err)
	}

	workDir := opts.WorkDir
	if workDir == "" {
		workDir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("app: resolve work dir: %w", err)
		}
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("app: resolve work dir: %w", err)
	}

	secrets := secret.New(filepath.Join(home, "secret"))

	memRoot := opts.MemoryRoot
	if memRoot == "" {
		// Global memory lives in ~/.chyan/memory (D12 revision 2026-10-16:
		// memory is chyan's own data and backs up with ~/.chyan; project
		// memory stays <repo>/.agents/memory so it travels with git).
		memRoot = filepath.Join(home, "memory")
	}
	mem := memory.New(memRoot)
	// The FTS5/inverted cache is derived and rebuildable; opening it must never
	// block boot, so a failure only degrades search to file scans.
	var memIndex *memory.Index
	if idx, err := memory.Open(filepath.Join(home, "memory-index.db")); err != nil {
		log.Printf("app: memory index degraded: %v", err)
	} else {
		mem.SetIndex(idx)
		memIndex = idx
	}

	st := opts.Store
	if st == nil {
		st, err = store.NewSQLite(filepath.Join(home, "chyan.db"))
		if err != nil {
			if memIndex != nil {
				_ = memIndex.Close()
			}
			return nil, fmt.Errorf("app: open store: %w", err)
		}
	}
	closeOpened := func() {
		_ = st.Close()
		if memIndex != nil {
			_ = memIndex.Close()
		}
	}

	backend := opts.Backend
	if backend == nil {
		backend, err = buildBackend(cfg, secrets)
		if err != nil {
			closeOpened()
			return nil, err
		}
	}

	core.SetToolOutputMaxBytes(cfg.Tools.ToolOutputMaxBytes)

	tracer := opts.Tracer
	if tracer == nil {
		// The production store persists spans; in-memory test stores simply
		// leave tracing disabled without changing any call path.
		var sink trace.Sink
		if ts, ok := st.(trace.Sink); ok {
			sink = ts
		}
		tracer = trace.NewRecorder(sink)
	}

	a := &App{
		cfg: cfg, workDir: workDir, home: home, memRoot: memRoot,
		store: st, memory: mem, secrets: secrets, backend: backend,
		memIndex: memIndex, tracer: tracer,
		windows:    windowConfig(cfg),
		reviewMode: reviewMode(cfg, opts.Auto),
		tools:      core.NewToolRegistry(), rawTools: map[string]core.Tool{},
		cfgPath: cfgPath, opts: opts, backendInjected: opts.Backend != nil,
	}
	if err := a.registerTools(); err != nil {
		closeOpened()
		return nil, err
	}
	a.loadExtensions(opts)
	if !opts.SkipGate {
		opts.Backend = backend // the resolved LLM client backs the risk reviewer
		gate, err := buildGate(cfg, opts)
		if err != nil {
			closeOpened()
			return nil, err
		}
		a.SetGate(gate)
		a.permChecker = buildChecker(cfg)
	}
	// Connect configured MCP servers at startup (architecture 4.8): one
	// server's failure degrades only that server's tools, never blocks boot.
	if err := a.connectMCP(); err != nil {
		log.Printf("app: mcp connect degraded: %v", err)
	}
	// Wire outbound IM channels for scheduled-job delivery (P4-2/P4-1 glue).
	if opts.Channels != nil {
		a.channels = opts.Channels
		a.deliver = deliver.NewRouter(opts.Channels)
	} else {
		a.connectChannels()
	}
	// Lazy startup maintenance: cache TTL purge + one dream pass
	// (architecture 4.6/4.3, never blocks boot).
	a.runStartupMaintenance()
	return a, nil
}

// connectChannels builds the channel registry from [channels.<name>]; each
// entry degrades independently and credential values go through secret
// resolution ("secret:<name>", D20).
func (a *App) connectChannels() {
	if len(a.cfg.Channels) == 0 {
		return
	}
	reg := channels.NewRegistry()
	for name, cc := range a.cfg.Channels {
		if cc.Disabled {
			continue
		}
		ch, err := buildChannel(name, cc, a.secrets)
		if err != nil {
			log.Printf("app: channel %s degraded: %v", name, err)
			continue
		}
		if err := reg.Register(ch); err != nil {
			log.Printf("app: channel %s register degraded: %v", name, err)
		}
	}
	a.channels = reg
	a.deliver = deliver.NewRouter(reg)
}

// buildChannel constructs one adapter, resolving "secret:<name>" credentials.
func buildChannel(name string, cc config.Channel, secrets *secret.Store) (channels.Channel, error) {
	resolve := func(v string) (string, error) {
		if secrets == nil || !strings.Contains(v, "secret:") {
			return v, nil
		}
		return secrets.ResolveSecrets(v)
	}
	inbound := channels.InboundPolicy{
		Enabled: cc.Inbound, AllowedChats: cc.AllowedChats, AllowedUsers: cc.AllowedUsers,
		MaxChars: cc.MaxMessageChars, MinInterval: cc.MinInterval.D(),
		MaxReplyChars: cc.MaxReplyChars, IntentFilter: cc.IntentFilter,
	}
	switch cc.Type {
	case "feishu", "":
		appID, err := resolve(cc.AppID)
		if err != nil {
			return nil, err
		}
		appSecret, err := resolve(cc.AppSecret)
		if err != nil {
			return nil, err
		}
		return channels.NewFeishu(channels.FeishuConfig{
			AppID: appID, AppSecret: appSecret, WebhookToken: cc.WebhookToken,
			ReceiveIDType: cc.ReceiveIDType, BaseURL: cc.BaseURL, Inbound: inbound,
		}), nil
	case "telegram":
		token, err := resolve(cc.Token)
		if err != nil {
			return nil, err
		}
		return channels.NewTelegram(channels.TelegramConfig{
			Token: token, BaseURL: cc.BaseURL, Inbound: inbound,
		}), nil
	default:
		return nil, fmt.Errorf("unknown channel type %q", cc.Type)
	}
}

// Deliver routes job output to its delivery target ("feishu:oc_x",
// "telegram:123"); the cron scheduler calls this through its Run callback.
func (a *App) Deliver(ctx context.Context, target, text string) error {
	if a.deliver == nil {
		return errors.New("app: no channels configured for delivery")
	}
	return a.deliver.Route(ctx, target, text)
}

// Channels exposes the channel registry (daemon webhook mounting).
func (a *App) Channels() *channels.Registry { return a.channels }

// connectMCP wires the configured MCP servers and registers their tools
// (named <server>_<tool>) alongside the built-ins.
func (a *App) connectMCP() error {
	if len(a.cfg.MCP.Servers) == 0 {
		return nil
	}
	cfgs := make([]mcp.ServerConfig, 0, len(a.cfg.MCP.Servers))
	for name, sc := range a.cfg.MCP.Servers {
		cfgs = append(cfgs, mcp.ServerConfig{
			Name: name, Type: sc.Type, URL: sc.URL, Command: sc.Command,
			Headers: sc.Headers, CatalogTimeout: sc.CatalogTimeout,
			ExecutionTimeout: sc.ExecutionTimeout, Disabled: sc.Disabled,
		})
	}
	// No Approver: gating happens in gatedTool (registryFor wraps MCP adapters
	// like built-ins), so the config rules, agent permission subset and audit
	// trail all apply to MCP calls too.
	a.mcp = mcp.New(cfgs, a.secrets, mcp.ToolOptions{
		MaxOutput: a.cfg.Tools.ToolOutputMaxBytes,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return a.mcp.Connect(ctx, a.tools)
}

// buildBackend resolves the LLM API keys and builds the routing layer: the
// primary [llm] endpoint first, then each [[llm.fallback]] entry in order
// (FR-1 fallback chain; the router walks the chain on transient failures).
func buildBackend(cfg config.Config, secrets *secret.Store) (core.LLMBackend, error) {
	if cfg.LLM.BaseURL == "" {
		return nil, errors.New("app: [llm].base_url is not configured")
	}
	resolveKey := func(key string) (string, error) {
		if key == "" {
			return "", nil
		}
		return secrets.ResolveSecrets(key)
	}
	type endpoint struct {
		name            string
		baseURL, apiKey string
		model           string
		streaming       bool
		headers         map[string]string
		costIn          float64
		costOut         float64
		costCached      float64
	}
	eps := []endpoint{{name: "primary", baseURL: cfg.LLM.BaseURL,
		apiKey: cfg.LLM.APIKey, model: cfg.LLM.Model, streaming: true,
		headers: cfg.LLM.Headers,
		costIn:  cfg.LLM.CostPer1MIn, costOut: cfg.LLM.CostPer1MOut,
		costCached: cfg.LLM.CostPer1MCached}}
	for i, fb := range cfg.LLM.Fallback {
		if fb.BaseURL == "" {
			// A broken entry must not widen or block the chain: skip it.
			log.Printf("app: llm.fallback[%d] missing base_url, skipped", i)
			continue
		}
		eps = append(eps, endpoint{
			name:    fmt.Sprintf("fallback-%d", i),
			baseURL: fb.BaseURL, apiKey: fb.APIKey,
			model: fb.Model, streaming: true,
			headers: fb.Headers,
			costIn:  fb.CostPer1MIn, costOut: fb.CostPer1MOut,
			costCached: fb.CostPer1MCached,
		})
	}
	specs := make([]llm.ModelSpec, 0, len(eps))
	clients := make(map[string]llm.Client, len(eps))
	for _, ep := range eps {
		apiKey, err := resolveKey(ep.apiKey)
		if err != nil {
			return nil, fmt.Errorf("app: resolve llm api key (%s): %w", ep.name, err)
		}
		// Header values follow the same "secret:<name>" convention as api_key,
		// so gateways can take a token from the secret store.
		headers := make(map[string]string, len(ep.headers))
		for k, v := range ep.headers {
			val, err := resolveKey(v)
			if err != nil {
				return nil, fmt.Errorf("app: resolve llm header %s (%s): %w", k, ep.name, err)
			}
			headers[k] = val
		}
		specs = append(specs, llm.ModelSpec{
			Provider:  ep.name,
			Model:     ep.model,
			Streaming: ep.streaming,
			// Config prices are per 1M (the published unit); specs per 1K.
			CostPer1KIn: ep.costIn / 1000, CostPer1KOut: ep.costOut / 1000,
			CostPer1KCached: ep.costCached / 1000,
		})
		clients[ep.name] = llm.NewOpenAIClient(llm.Provider{
			Name:    ep.name,
			BaseURL: ep.baseURL,
			APIKey:  apiKey,
			Headers: headers,
		}, ep.model)
	}
	return llm.NewRouter(specs, clients, windowConfig(cfg)), nil
}

// windowConfig maps [context] window fields to the layered window resolver
// (config > online catalog > builtin table > fallback; FR-1). The catalog
// cache lives in the chyan cache dir (D12: ~/.chyan/cache).
func windowConfig(cfg config.Config) *llm.WindowConfig {
	wc := &llm.WindowConfig{
		ContextLimit:    cfg.Context.ContextLimit,
		DefaultLimit:    cfg.Context.DefaultLimit,
		OnlineCatalog:   cfg.Context.OnlineCatalog,
		WindowTablePath: cfg.Context.WindowTable,
	}
	if home, err := config.Home(); err == nil {
		wc.CatalogCache = filepath.Join(home, "cache", "model_catalog.json")
	}
	return wc
}

// registerTools wires file/bash tools, memory tools and MCP adapters into the
// app's shared registry.
func (a *App) registerTools() error {
	reg, raw, err := a.buildTools(a.cfg)
	if err != nil {
		return err
	}
	a.tools, a.rawTools = reg, raw
	return nil
}

// buildTools assembles a registry and its raw tool map for cfg without
// mutating the app. Reload builds the new set here first so a registration
// failure leaves the live configuration untouched.
func (a *App) buildTools(cfg config.Config) (*core.ToolRegistry, map[string]core.Tool, error) {
	reg := core.NewToolRegistry()
	raw := make(map[string]core.Tool)
	for _, t := range a.builtinTools(cfg, a.workDir) {
		wrapped := &gatedTool{
			inner: t, app: a,
			mode:          cfg.Review.Mode,
			needsApproval: !readOnlyTools[t.Name()],
		}
		if err := reg.Register(wrapped); err != nil {
			return nil, nil, fmt.Errorf("app: register %s: %w", t.Name(), err)
		}
		raw[t.Name()] = t
	}
	// task exposes sub-agent dispatch to the model (architecture 4.1.1).
	// Dispatch itself is not destructive: the child runs the same gated
	// tool chain for its own operations.
	task := core.NewTaskTool(a)
	if err := reg.Register(&gatedTool{
		inner: task, app: a, mode: cfg.Review.Mode,
		needsApproval: false,
	}); err != nil {
		return nil, nil, fmt.Errorf("app: register task: %w", err)
	}
	raw[task.Name()] = task
	return reg, raw, nil
}

// builtinTools constructs the workspace-bound tools for one root. Tools are
// cheap structs, so a per-task workspace gets a fresh set instead of sharing
// the startup root.
func (a *App) builtinTools(cfg config.Config, workDir string) []core.Tool {
	maxOut := cfg.Tools.ToolOutputMaxBytes
	bashTimeout := time.Duration(cfg.Tools.BashTimeout)
	return []core.Tool{
		tools.NewReadTool(workDir, maxOut),
		tools.NewWriteTool(workDir, maxOut),
		tools.NewEditTool(workDir, maxOut),
		tools.NewListTool(workDir, maxOut),
		tools.NewSearchTool(workDir, maxOut),
		tools.NewGrepTool(workDir, maxOut),
		tools.NewDeleteTool(workDir, maxOut),
		tools.NewBashTool(workDir, bashTimeout, maxOut),
		core.NewRememberTool(a),
		core.NewRecallTool(a),
		core.NewReadSessionTool(a),
		// P1 chained recall: pull the full body behind a truncated-output
		// pointer without re-running the original tool.
		core.NewExpandTool(a),
		// P4-4/5 capability tools: browser bridge + web fetch/search
		// (proxy via HTTP(S)_PROXY env, SSRF guard inside).
		tools.NewBrowserTool("", filepath.Join(a.home, "cache"), 0, maxOut),
		tools.NewWebFetchTool(bashTimeout, maxOut),
		tools.NewWebSearchTool(tools.NewDuckDuckGoProvider(bashTimeout), bashTimeout, maxOut),
		// P5-1 media tools: local Kokoro TTS + transcriber service.
		tools.NewTTSTool(workDir, filepath.Join(a.home, "cache"), 0, maxOut),
		tools.NewTranscribeTool(workDir, 0, maxOut),
	}
}

// rawToolsFor returns the raw tool set for a run workspace: the startup set
// when the workspace is unchanged, otherwise a fresh workspace-bound set.
func (a *App) rawToolsFor(workDir string) map[string]core.Tool {
	if workDir == "" || workDir == a.workDir {
		return a.rawTools
	}
	raw := make(map[string]core.Tool, len(a.rawTools))
	for _, t := range a.builtinTools(a.cfg, workDir) {
		raw[t.Name()] = t
	}
	raw["task"] = a.rawTools["task"]
	return raw
}

// registryFor builds one run's tool view: every raw tool re-wrapped with the
// run's session (audit trail), workspace root and agent permission subset
// (FR-5). Tools that only exist in the shared registry (MCP adapters) pass
// through unchanged.
func (a *App) registryFor(session string, perms map[string]core.Effect, workDir string) *core.ToolRegistry {
	reg := core.NewToolRegistry()
	raw := a.rawToolsFor(workDir)
	// Keep the wrapper's workDir empty for the startup root so diff previews
	// read a.workDir dynamically (tests and late workspace re-binding rely on
	// that); only an alternate root is pinned explicitly.
	previewDir := workDir
	if previewDir == a.workDir {
		previewDir = ""
	}
	for name, t := range raw {
		if name == "task" {
			continue // bound to the specific AgentImpl in buildAgentWith
		}
		_ = reg.Register(&gatedTool{
			inner: t, app: a, session: session, perms: perms, workDir: previewDir,
			mode:          a.cfg.Review.Mode,
			needsApproval: !readOnlyTools[name],
		})
	}
	for _, t := range a.tools.Tools() {
		if _, isRaw := raw[t.Name()]; isRaw {
			continue
		}
		// Tools that only live in the shared registry (MCP adapters) are
		// wrapped here as well: nothing may reach the model ungated.
		_ = reg.Register(&gatedTool{
			inner: t, app: a, session: session, perms: perms, workDir: previewDir,
			mode:          a.cfg.Review.Mode,
			needsApproval: !readOnlyTools[t.Name()],
		})
	}
	return reg
}

// MCPAvailable reports the live connection state per configured MCP server
// (nil when no server is configured). Observability only.
func (a *App) MCPAvailable() map[string]bool {
	a.wireMu.RLock()
	defer a.wireMu.RUnlock()
	if a.mcp == nil {
		return nil
	}
	return a.mcp.Available()
}

// MCPToolCounts reports how many adapters each MCP server registered (nil when
// no server is configured).
func (a *App) MCPToolCounts() map[string]int {
	a.wireMu.RLock()
	defer a.wireMu.RUnlock()
	if a.mcp == nil {
		return nil
	}
	return a.mcp.ToolCounts()
}

// Close releases the store and MCP connections.
func (a *App) Close() error {
	if a.mcp != nil {
		a.mcp.Close()
	}
	if a.memIndex != nil {
		_ = a.memIndex.Close()
	}
	return a.store.Close()
}

// WorkDir exposes the tool sandbox root.
func (a *App) WorkDir() string { return a.workDir }

// ArtifactRoot is the spill directory for oversized tool output.
func (a *App) ArtifactRoot() string { return a.artifactRoot() }

// Memory exposes the long-term memory store for the management view
// (list/get/supersede); agent recall keeps using the adapters above.
func (a *App) Memory() *memory.FileStore { return a.memory }

// Home exposes the resolved chyan data root (daemon wiring).
func (a *App) Home() string { return a.home }

// DaemonLimits returns the configured background-dispatch limits
// ([daemon] max_concurrent_tasks / task_timeout). Zero means "use the
// daemon's own default".
func (a *App) DaemonLimits() (maxConcurrent int, taskTimeout time.Duration) {
	a.wireMu.RLock()
	defer a.wireMu.RUnlock()
	return a.cfg.Daemon.MaxConcurrentTasks, a.cfg.Daemon.TaskTimeout.D()
}

// ReviewMode reports the effective review mode (human/ai/full) for status
// surfaces; --auto already resolves to "full".
func (a *App) ReviewMode() core.ReviewMode {
	a.wireMu.RLock()
	defer a.wireMu.RUnlock()
	return a.reviewMode
}

// Registry exposes the tool registry (MCP registration happens here).
func (a *App) Registry() *core.ToolRegistry {
	a.wireMu.RLock()
	defer a.wireMu.RUnlock()
	return a.tools
}

// Commands lists the loaded slash commands (chat UI /help).
func (a *App) Commands() []commands.Command {
	a.wireMu.RLock()
	defer a.wireMu.RUnlock()
	return a.commands
}

// StoreOf exposes the session store (daemon wiring).
func StoreOf(a *App) store.Store { return a.store }

// ImportHermesMemory imports one hermes memory markdown (~/.hermes/memories/
// USER.md or MEMORY.md) into the global memory root. Returns the number of
// entries migrated (memory ImportHermes semantics).
func (a *App) ImportHermesMemory(ctx context.Context, path string) (int, error) {
	return a.memory.ImportHermes(ctx, path)
}

// ------------------------------------------------------------- agent runs

// Run executes one user turn in the executor phase (ReAct when no plan). An
// empty sessionID starts a new session. A leading "/name args" slash
// invocation is expanded to its command template first.
func (a *App) Run(ctx context.Context, sessionID, userInput string) (core.Message, string, error) {
	return a.RunOpts(ctx, sessionID, userInput, RunOptions{})
}

// RunStream executes one user turn like Run, forwarding live token deltas to
// sink as they arrive (TUI/PWA realtime output). Concurrent-safe: the sink
// is per-call, not shared state.
func (a *App) RunStream(ctx context.Context, sessionID, userInput string, sink func(string)) (core.Message, string, error) {
	return a.RunOpts(ctx, sessionID, userInput, RunOptions{Sink: sink})
}

// RunStreamProgress is RunStream plus run-activity callbacks (thinking /
// tool start/end) so a UI can show what is happening during silent phases.
func (a *App) RunStreamProgress(ctx context.Context, sessionID, userInput string, sink func(string), onProgress func(core.ProgressEvent)) (core.Message, string, error) {
	return a.RunOpts(ctx, sessionID, userInput, RunOptions{Sink: sink, OnProgress: onProgress})
}

// RunOptions configure one agent turn. Phase selects the run mode (empty =
// executor). Agent optionally pins the run to one agent definition without
// mutating the process-wide setting (daemon tasks). Sink receives live token
// deltas; OnProgress receives run activity.
// ImageInput is one user-supplied image for a turn (CLI --image, PWA upload,
// IM attachment). MIME may be empty; the bytes are sniffed.
type ImageInput struct {
	MIME string
	Data []byte
}

type RunOptions struct {
	Phase      core.Phase
	Agent      string
	Sink       func(string)
	OnProgress func(core.ProgressEvent)
	// Images are attached to this turn's user message (max 4, each ≤5MB).
	Images []ImageInput
	// OnInterruptReady receives a function that interrupts this run (LLM
	// calls, tools, sub-agents) without cancelling ctx, so messages and tool
	// results completed before the interrupt still persist. Called once,
	// before the first LLM call; nil means the run cannot be interrupted
	// cooperatively.
	OnInterruptReady func(interrupt func())
	// NoTools runs the turn with an empty tool registry: the session can talk
	// and reason but never act. Used by the always-on router layer, which must
	// dispatch work instead of executing it.
	NoTools bool
	// SystemOverride replaces the system prompt for this turn and disables
	// triggered-lesson injection and the skills/directives appendix, so a
	// caller-supplied prompt is used exactly as written.
	SystemOverride string
	// TaskID links this turn to a daemon/cron task in trace rows (FR-13).
	TaskID string
	// WorkDir selects the workspace root for this turn. Empty keeps the
	// daemon default; a session's recorded workspace is supplied by the caller.
	WorkDir string
	// Model overrides the run model for this turn ("" = config/agent default).
	Model string
	// Retry replays the session's last user turn instead of appending a new
	// user message; failed assistant/tool rows after it are skipped for this
	// run. Used by the Web "retry" action.
	Retry bool
}

// workDirKey carries a per-run workspace root into tools/adapters that read
// the project scope (memory recall, lessons) without threading it everywhere.
type workDirKey struct{}

func withRunWorkDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, workDirKey{}, dir)
}

// runWorkDir resolves the workspace for this turn, falling back to the App's
// startup directory when no override is present.
func (a *App) runWorkDir(ctx context.Context) string {
	if dir, ok := ctx.Value(workDirKey{}).(string); ok && dir != "" {
		return dir
	}
	return a.workDir
}

// ResolveWorkDir validates a per-task workspace path (daemon and file
// endpoints reuse the agent's rules so they cannot diverge).
func ResolveWorkDir(base, requested string) (string, error) {
	return resolveWorkDir(base, requested)
}

// resolveWorkDir validates a per-task workspace. The daemon trusts its token
// holder, but a path must still exist and be a directory; symlinks are
// resolved so tools never sandbox against a link that later moves.
func resolveWorkDir(base, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return base, nil
	}
	abs, err := filepath.Abs(requested)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve %q: %w", requested, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace: %s is not a directory", abs)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return abs, nil
}

// RunOpts executes one user turn under the given options. Planner phase runs
// read-only (filtered tools) against the built-in planner prompt to draft a
// write plan; executor phase injects any finalized plan and carries it out on
// an execution-intent turn (ReAct when no plan exists).
func (a *App) RunOpts(ctx context.Context, sessionID, userInput string, opts RunOptions) (core.Message, string, error) {
	// One turn holds the read side of runMu for its whole lifetime: Reload takes
	// the write side (TryLock) so a config swap can never land mid-run.
	a.runMu.RLock()
	defer a.runMu.RUnlock()
	expanded, err := a.expandCommand(userInput)
	if err != nil {
		return core.Message{}, "", err
	}
	// Resolve per-run settings before creating a session: its initial Agent
	// and Model metadata must describe this run even when the LLM call fails.
	prompt, agentModel, agentPerms, override := a.agentSettings()
	if opts.Agent != "" {
		body, model, perms, aerr := a.loadAgentDef(opts.Agent)
		if aerr != nil {
			return core.Message{}, "", aerr
		}
		prompt, agentModel, agentPerms = body, model, perms
	}
	if opts.Model != "" {
		override = opts.Model
	}
	model := modelFor(a.cfg.LLM.Model, agentModel, override)
	workDir, err := resolveWorkDir(a.workDir, opts.WorkDir)
	if err != nil {
		return core.Message{}, "", err
	}
	// Project memory and directives follow the selected workspace, so set it
	// on the context before any project-scoped read happens.
	ctx = withRunWorkDir(ctx, workDir)
	sid, err := a.resolveSessionForRun(ctx, sessionID, opts.Agent, model)
	if err != nil {
		return core.Message{}, "", err
	}
	// One trace per turn: memory injection and the agent loop must share it.
	ctx = trace.WithTrace(ctx, sid, opts.TaskID, sid)
	// Trigger-matched lessons are injected on the user channel before the turn
	// so a past pitfall resurfaces without an explicit recall. Retrieved
	// memory never enters the system prompt (prompt-injection defence). A
	// caller that supplied its own prompt (router) opts out of this injection.
	lessons := ""
	if opts.SystemOverride == "" {
		lessons = a.lessonsFor(ctx, expanded)
	} else {
		prompt = opts.SystemOverride
	}
	history, err := a.loadHistory(ctx, sid)
	if err != nil {
		return core.Message{}, "", err
	}
	// Retry replays the session's last user turn: remove it from history so it
	// is not sent twice, and skip the failed assistant/tool rows after it.
	var retryMsg *core.Message
	if opts.Retry {
		idx := -1
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Role == core.RoleUser {
				idx = i
				break
			}
		}
		if idx < 0 {
			return core.Message{}, sid, errors.New("app: retry needs a previous user turn")
		}
		msg := history[idx]
		retryMsg = &msg
		history = history[:idx]
	}
	// The executor carries the finalized plan; the planner drafts it.
	var plan core.Plan
	if opts.Phase != core.PhasePlanner {
		plan, _ = a.LoadPlan(ctx, sid)
	}
	sysPrompt := a.promptWithSkillsFor(prompt, workDir)
	if opts.NoTools {
		// A tool-less turn cannot use the skills appendix (skills are tool
		// backed), so the caller's prompt is used exactly as written.
		sysPrompt = prompt
	}
	agent, _ := a.buildAgentWith(sid, history, opts.Sink, agentPerms, sysPrompt,
		model, override, opts.Phase, plan, opts.NoTools, workDir)
	if opts.OnInterruptReady != nil {
		opts.OnInterruptReady(agent.Interrupt)
	}
	var turn core.ProgressEvent // final usage snapshot for this turn
	agent.OnProgress(func(ev core.ProgressEvent) {
		if ev.Kind == core.ProgressDone {
			turn = ev
		}
		if opts.OnProgress != nil {
			opts.OnProgress(ev)
		}
	})
	// The user message belongs to history before the turn runs. Persist through
	// the session-bound sink, never through a.currentSession: concurrent runs
	// (daemon background tasks, sub-agents) overwrite that field and their
	// messages would land in the wrong session.
	var userMsg core.Message
	if retryMsg != nil {
		// The row is already persisted; replay it without appending a second
		// copy. Its image parts keep the original attachments.
		userMsg = *retryMsg
	} else {
		parts := []core.Part{{Type: "text", Text: expanded + lessons}}
		if len(opts.Images) > 0 {
			if len(opts.Images) > core.MaxImagesPerTurn {
				return core.Message{}, sid, fmt.Errorf("app: at most %d images per turn", core.MaxImagesPerTurn)
			}
			if strings.EqualFold(a.cfg.LLM.Vision, "off") {
				return core.Message{}, sid, errors.New("app: image input disabled ([llm] vision = \"off\"); " +
					"configure a vision-capable model or use OCR/transcription instead")
			}
			for _, in := range opts.Images {
				part, err := core.SaveImage(filepath.Join(a.home, "artifacts"), sid, in.MIME, in.Data)
				if err != nil {
					return core.Message{}, sid, fmt.Errorf("app: image: %w", err)
				}
				parts = append(parts, part)
			}
		}
		userMsg = core.Message{Role: core.RoleUser, Content: parts, Timestamp: time.Now().UTC()}
		if err := (&sessionSink{app: a, sessionID: sid}).AppendMessage(ctx, userMsg); err != nil {
			return core.Message{}, sid, err
		}
	}

	msg, err := agent.RunMessage(ctx, userMsg)
	if err != nil {
		// Persist how the turn ended: a user interrupt leaves the session
		// interrupted (visible in listings); other failures keep the previous
		// status so a transient API error does not erase it.
		if errors.Is(err, core.ErrInterrupted) {
			a.setSessionStatus(ctx, sid, store.SessionStatusInterrupted)
		}
		return core.Message{}, sid, err
	}
	a.setSessionStatus(ctx, sid, store.SessionStatusActive)
	if err := a.addSessionUsage(ctx, sid, turn); err != nil {
		return msg, sid, err
	}
	a.titleSession(ctx, sid, expanded, msg) // first turn names the session
	return msg, sid, nil
}

// setSessionStatus records the run status on the session row (best effort).
// The write must survive cancellation and daemon shutdown, so it detaches
// from ctx: an interrupt cancels the run context, not the bookkeeping.
func (a *App) setSessionStatus(ctx context.Context, sessionID, status string) {
	if sessionID == "" || status == "" {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s, err := a.store.GetSession(wctx, sessionID)
	if err != nil {
		return
	}
	if s.Status == status {
		return
	}
	s.Status = status
	s.UpdatedAt = time.Now().UTC()
	_ = a.store.UpdateSession(wctx, s)
}

// Status reports window usage for a session's current history.
func (a *App) Status(ctx context.Context, sessionID string) (core.Budget, error) {
	// The budget reads [context] thresholds: guard against a concurrent reload
	// swapping them (status is served from daemon goroutines).
	a.wireMu.RLock()
	defer a.wireMu.RUnlock()
	sid, err := a.resolveSession(ctx, sessionID)
	if err != nil {
		return core.Budget{}, err
	}
	history, err := a.loadHistory(ctx, sid)
	if err != nil {
		return core.Budget{}, err
	}
	return a.contextBudget(sid, history), nil
}

// SessionDetail returns the transcript slice and its live window budget,
// loading the stored history once. Callers that already need the transcript
// (the Web session view) use this instead of ReadSession + Status, which would
// read the same hundreds of rows twice on every refresh.
func (a *App) SessionDetail(ctx context.Context, sessionID string, transcriptLimit int) ([]core.Message, core.Budget, error) {
	sid, err := a.resolveSession(ctx, sessionID)
	if err != nil {
		return nil, core.Budget{}, err
	}
	history, err := a.loadHistory(ctx, sid)
	if err != nil {
		return nil, core.Budget{}, err
	}
	budget := a.contextBudget(sid, history)
	if transcriptLimit > 0 && len(history) > transcriptLimit {
		history = history[:transcriptLimit]
	}
	return history, budget, nil
}

// contextBudget assembles one context manager over supplied history. The
// history is already loaded, so usage reporting does not re-read the session.
func (a *App) contextBudget(sessionID string, history []core.Message) core.Budget {
	_, ctxMgr := a.buildAgent(sessionID, history, nil, nil, core.PhaseExecutor, core.Plan{})
	return ctxMgr.Status()
}

// Compact folds a session's history into a handoff note and persists it.
func (a *App) Compact(ctx context.Context, sessionID string) (core.HandoffNote, error) {
	sid, err := a.resolveSession(ctx, sessionID)
	if err != nil {
		return core.HandoffNote{}, err
	}
	history, err := a.loadHistory(ctx, sid)
	if err != nil {
		return core.HandoffNote{}, err
	}
	_, ctxMgr := a.buildAgent(sid, history, nil, nil, core.PhaseExecutor, core.Plan{})
	return ctxMgr.Compact(ctx)
}

// AppendShellNote records a user-run shell command (the "!" REPL escape) and
// its output as a user message, so later turns see what was executed without
// paying an LLM call. The note is user-authored — no approval gate applies.
// Output is clamped to the tool-output budget like any tool result.
func (a *App) AppendShellNote(ctx context.Context, sessionID, cmd, output string) (string, error) {
	sid, err := a.resolveSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	text := "[shell] $ " + cmd + "\n" + output
	if max := core.ToolOutputMaxBytes(); len(text) > max {
		text = text[:max] + "\n…(已截断)"
	}
	sink := sessionSink{app: a, sessionID: sid}
	err = sink.AppendMessage(ctx, core.Message{
		Role:      core.RoleUser,
		Content:   []core.Part{{Type: "text", Text: text}},
		Timestamp: time.Now(),
	})
	if err != nil {
		return sid, err
	}
	if err := a.touchSession(ctx, sid); err != nil {
		return sid, err
	}
	return sid, nil
}

// buildAgent assembles the context manager and agent loop for one session.
// sink, when non-nil, receives live token deltas (TUI/PWA streaming). perms
// is the agent-level permission subset (FR-5): nil means unconstrained. phase
// and plan select the run mode (planner read-only vs executor with plan).
func (a *App) buildAgent(sessionID string, history []core.Message, sink func(string), perms map[string]core.Effect, phase core.Phase, plan core.Plan) (*core.AgentImpl, *core.ContextManagerImpl) {
	prompt, agentModel, _, override := a.agentSettings()
	return a.buildAgentWith(sessionID, history, sink, perms, a.promptWithSkills(prompt),
		modelFor(a.cfg.LLM.Model, agentModel, override), override, phase, plan, false, a.workDir)
}

// buildSubAgent builds a child agent with its own prompt, model and narrowed
// permissions (architecture 4.1.1). Children always run the executor phase.
func (a *App) buildSubAgent(sessionID, prompt, model, modelOverride string, perms map[string]core.Effect, workDir string) (*core.AgentImpl, *core.ContextManagerImpl) {
	if prompt == "" {
		prompt = a.promptWithSkillsFor(a.systemPromptBase(), workDir)
	}
	return a.buildAgentWith(sessionID, nil, nil, perms, prompt, model, modelOverride, core.PhaseExecutor, core.Plan{}, false, workDir)
}

// systemPromptBase returns the configured default prompt without the appended
// skills/directives blocks.
func (a *App) systemPromptBase() string {
	base, _, _, _ := a.agentSettings()
	return base
}

// modelFor picks the effective model: config default, then agent definition,
// then the per-run override (most specific wins).
func modelFor(configured, agentModel, override string) string {
	m := configured
	if agentModel != "" {
		m = agentModel
	}
	if override != "" {
		m = override
	}
	return m
}

// buildAgentWith is the shared assembly with explicit prompt/model. Phase
// selects the tool set and prompt: planner is read-only with the built-in
// planner prompt; executor injects the finalized plan (if any) or falls back
// to the default ReAct prompt. noTools builds a tool-less turn (router): the
// registry is empty, no task tool and no sub-agent dispatcher are attached,
// and phase-based prompt/tool rewriting is skipped.
func (a *App) buildAgentWith(sessionID string, history []core.Message, sink func(string), perms map[string]core.Effect, prompt, model, modelOverride string, phase core.Phase, plan core.Plan, noTools bool, workDir string) (*core.AgentImpl, *core.ContextManagerImpl) {
	if workDir == "" {
		workDir = a.workDir
	}
	reg := core.NewToolRegistry()
	dispatcher := &runTaskDispatcher{}
	if !noTools {
		reg = a.registryFor(sessionID, perms, workDir)
		previewDir := workDir
		if previewDir == a.workDir {
			previewDir = ""
		}
		_ = reg.Register(&gatedTool{
			inner: core.NewTaskTool(dispatcher), app: a, session: sessionID, perms: perms,
			workDir: previewDir, mode: a.cfg.Review.Mode, needsApproval: false,
		})
		switch phase {
		case core.PhasePlanner:
			// Read-only planning: drop write tools, guard bash, add submit_plan,
			// and use the built-in planner prompt (non-overridable by design).
			reg = core.PlannerRegistry(reg, core.NewSubmitPlanTool(a, sessionID))
			prompt = core.PlannerSystemPrompt
		default:
			if !plan.Empty() {
				// Executor carrying a finalized plan: built-in executor prompt plus
				// the plan (intent gate and deviation reporting live in the prompt).
				prompt = core.ExecutorSystemPrompt + core.RenderPlan(plan)
			}
		}
	}
	toolDefs := make([]map[string]any, 0, 64)
	for _, t := range reg.Tools() {
		toolDefs = append(toolDefs, t.Schema())
	}
	// FR-1 layered window resolution: config > online catalog > builtin
	// table > fallback, keyed by the effective run model.
	contextLimit := llm.DefaultFallbackLimit
	if a.windows != nil {
		contextLimit, _ = a.windows.ResolveContextLimit(model)
	}
	ctxCfg := core.ContextConfig{
		ContextLimit:        contextLimit,
		ReservedTokens:      a.cfg.Context.ReservedTokens,
		TriggerRatio:        a.cfg.Context.TriggerRatio,
		TriggerBytes:        a.cfg.Context.TriggerBytes,
		CompressTargetRatio: a.cfg.Context.CompressTargetRatio,
		MaxRecentTurns:      a.cfg.Context.MaxRecentTurns,
		SummaryRatio:        a.cfg.Context.SummaryRatio,
		SummaryMaxRatio:     a.cfg.Context.SummaryMaxRatio,
		FoldToolBytes:       a.cfg.Context.FoldToolBytes,
	}
	deps := core.ContextDeps{
		Counter:    llm.NewCounter(a.home),
		Compressor: core.TruncateCompressor{},
		Extractor:  core.HeuristicExtractor{},
		Writer:     a,
		Mirror:     a,
		SessionID:  sessionID,
	}
	ctxMgr := core.NewContextManager(ctxCfg, prompt, toolDefs, deps)
	if err := ctxMgr.Append(history...); err != nil {
		// Append only fails on nil input today; keep the session usable.
		_ = err
	}
	// The message sink is bound to this session: concurrent runs (daemon
	// background tasks, sub-agents) must not cross-persist into a shared
	// currentSession field.
	msgSink := &sessionSink{app: a, sessionID: sessionID}
	agent := core.NewAgent(a.backend, ctxMgr, reg, msgSink, sessionID, core.AgentConfig{
		Model:                   model,
		MaxToolIterations:       a.cfg.Tools.MaxToolIterations,
		MaxToolRelays:           a.cfg.Tools.MaxToolRelays,
		MaxSubAgents:            4,
		MaxNestDepth:            2,
		MaxRepeatedToolFailures: a.cfg.Tools.MaxRepeatedToolFailures,
		ArtifactDir:             a.artifactRoot(),
	})
	agent.SetTracer(a.tracer)
	if !noTools {
		dispatcher.agent = agent
		agent.AttachSubAgents(a.subAgentRunner(), core.AgentContext{
			SessionID: sessionID, Depth: 0, Perms: perms, Model: model, ModelOverride: modelOverride,
			WorkDir: workDir,
		})
	}
	if sink != nil {
		agent.OnDelta(func(d core.Delta) {
			if d.Text != "" {
				sink(d.Text)
			}
		})
	}
	return agent, ctxMgr
}

// runTaskDispatcher breaks the construction cycle between an AgentImpl and
// its task tool. Each run owns one dispatcher; agent is set before Run starts.
type runTaskDispatcher struct{ agent *core.AgentImpl }

func (d *runTaskDispatcher) RunTask(ctx context.Context, req core.TaskRequest) (core.TaskResult, error) {
	return d.agent.RunTask(ctx, req)
}

// sessionSink persists turns into one fixed session (core.MessageSink).
type sessionSink struct {
	app       *App
	sessionID string
}

// AppendMessage implements core.MessageSink for one session.
func (s *sessionSink) AppendMessage(ctx context.Context, msg core.Message) error {
	id, err := newID("msg")
	if err != nil {
		return err
	}
	return s.app.store.AppendMessage(ctx, store.Message{
		ID:        id,
		SessionID: s.sessionID,
		Role:      string(msg.Role),
		Content:   marshalMessage(msg),
		CreatedAt: msg.Timestamp,
	})
}

// subAgentRunner lazily builds the shared sub-agent dispatcher. Children get
// their own session (ParentID linked) and their own message sink.
func (a *App) subAgentRunner() *core.SubAgentRunner {
	a.subOnce.Do(func() {
		a.subRunner = core.NewSubAgentRunner(core.SubAgentConfig{}, core.SubAgentDeps{
			MaxToolIterations: 15,
			NewAgent: func(ctx context.Context, req core.TaskRequest, parent core.AgentContext) (*core.AgentImpl, error) {
				childID, err := newID("ses")
				if err != nil {
					return nil, err
				}
				// Use the parent run's captured settings, never process-wide
				// agentSettings: daemon runs may pin different agents concurrently.
				perms := parent.Perms
				childPrompt := ""
				childModel := ""
				if req.AgentName != "" {
					body, model, decls, err := a.loadAgentDef(req.AgentName)
					if err != nil {
						return nil, err
					}
					childPrompt = body
					childModel = model
					perms = mergePerms(perms, decls)
				}
				model := modelFor(parent.Model, childModel, parent.ModelOverride)
				if err := a.store.CreateSession(ctx, store.Session{
					ID: childID, ParentID: parent.SessionID, Agent: req.AgentName,
					Model:     model,
					CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
				}); err != nil {
					return nil, err
				}
				agent, _ := a.buildSubAgent(childID, childPrompt, model, parent.ModelOverride, perms, parent.WorkDir)
				agent.AttachSubAgents(a.subRunner, core.AgentContext{
					SessionID: childID, Depth: parent.Depth + 1, Perms: perms,
					Model: model, ModelOverride: parent.ModelOverride, WorkDir: parent.WorkDir,
				})
				return agent, nil
			},
		})
		a.subRunner.SetTracer(a.tracer)
	})
	return a.subRunner
}

// RunTask dispatches one sub-agent (core.TaskDispatcher, used by the task
// tool's fallback path; the agent loop binds its own session via RunTask on
// the engine).
func (a *App) RunTask(ctx context.Context, req core.TaskRequest) (core.TaskResult, error) {
	_, agentModel, perms, override := a.agentSettings()
	model := modelFor(a.cfg.LLM.Model, agentModel, override)
	return a.subAgentRunner().Dispatch(ctx, req, core.AgentContext{
		SessionID: a.CurrentSession(), Depth: 0, Perms: perms,
		Model: model, ModelOverride: override, WorkDir: a.runWorkDir(ctx),
	})
}

// systemPrompt is the default agent instruction. --agent overrides this with
// an .agents/agents/<name>.md body (see SetAgent). The skill inventory is
// appended so the model knows which skills exist (architecture 4.4: bodies
// load just-in-time, never all at once).
func (a *App) systemPrompt() string {
	base, _, _, _ := a.agentSettings()
	return a.promptWithSkills(base)
}

// promptWithSkills appends the skill inventory to a base prompt.
func (a *App) promptWithSkills(base string) string {
	return appendSkills(base, a.skills, a.directives)
}

// promptWithSkillsFor renders the prompt for one workspace. Directives
// (AGENTS.md/CLAUDE.md) are workspace-specific; the skill inventory stays the
// process-wide set loaded at startup.
func (a *App) promptWithSkillsFor(base, workDir string) string {
	if workDir == "" || workDir == a.workDir {
		return a.promptWithSkills(base)
	}
	return appendSkills(base, a.skills, a.directivesFor(workDir))
}

// directivesFor loads instructions for an alternate workspace root. The
// startup root keeps its already-rendered value.
func (a *App) directivesFor(workDir string) string {
	if !a.cfg.Context.LoadProjectInstructions {
		return ""
	}
	base := ""
	if userHome, err := os.UserHomeDir(); err == nil {
		base = filepath.Join(userHome, ".agents")
	}
	return instructions.Render(instructions.Load(base, workDir))
}

// appendSkills builds the system prompt from the base text, skill inventory
// and workspace directives.
func appendSkills(base string, skills []skills.Skill, directives string) string {
	if base == "" {
		base = "You are chyan, a capable personal agent. Use tools to inspect and " +
			"modify files under the work directory. Be concise and concrete."
	}
	if len(skills) == 0 {
		return base + directives
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\n## Available skills\nInvoke a skill by name when its description matches the task.\n")
	for _, sk := range skills {
		b.WriteString("- ")
		b.WriteString(sk.Name)
		if sk.Description != "" {
			b.WriteString(": ")
			b.WriteString(sk.Description)
		}
		b.WriteString("\n")
	}
	b.WriteString(directives)
	return b.String()
}

// expandCommand rewrites a leading "/name args" slash invocation into the
// command's rendered prompt template. Non-command input passes through.
func (a *App) expandCommand(input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "/") {
		return input, nil
	}
	name, args := splitSlash(trimmed)
	for _, c := range a.commands {
		if c.Name == name {
			return commands.Render(c, args)
		}
	}
	// Unknown slash stays as literal input (some prompts start with "/").
	return input, nil
}

// splitSlash splits "/name rest" into name and the remaining arguments.
func splitSlash(s string) (name, args string) {
	body := strings.TrimPrefix(s, "/")
	if i := strings.IndexAny(body, " \t"); i >= 0 {
		return body[:i], strings.TrimSpace(body[i+1:])
	}
	return body, ""
}

// SetAgent loads an agent definition as the system prompt for subsequent
// runs (CLI --agent). An empty name restores the default prompt.
func (a *App) SetAgent(name string) error {
	body, model, perms, err := "", "", map[string]core.Effect(nil), error(nil)
	if name != "" {
		body, model, perms, err = a.loadAgentDef(name)
		if err != nil {
			return err
		}
	}
	a.agentMu.Lock()
	a.prompt, a.agentModel, a.agentPerms = body, model, perms
	a.agentMu.Unlock()
	return nil
}

// SetModel overrides the configured model for subsequent runs (empty
// restores the config default).
func (a *App) SetModel(model string) {
	a.agentMu.Lock()
	a.modelOverride = model
	a.agentMu.Unlock()
}

// agentSettings snapshots the run-time agent knobs: concurrent runs must not
// observe a half-applied SetAgent, and daemon tasks pass their agent per run
// instead of mutating this global.
func (a *App) agentSettings() (prompt, model string, perms map[string]core.Effect, override string) {
	a.agentMu.RLock()
	defer a.agentMu.RUnlock()
	return a.prompt, a.agentModel, a.agentPerms, a.modelOverride
}

// ForkSession copies srcID's transcript into a fresh session (FR-4 --fork)
// and returns the new session id. The fork carries the source's agent and
// model metadata and is not linked as a child (it is an editable branch).
func (a *App) ForkSession(ctx context.Context, srcID string) (string, error) {
	src, err := a.store.GetSession(ctx, srcID)
	if err != nil {
		return "", fmt.Errorf("app: fork source %q: %w", srcID, err)
	}
	sid, err := newID("ses")
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if err := a.store.CreateSession(ctx, store.Session{
		ID: sid, Agent: src.Agent, Model: src.Model,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return "", err
	}
	rows, err := a.store.ListMessages(ctx, srcID, 10000, 0)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		id, err := newID("msg")
		if err != nil {
			return "", err
		}
		row.ID = id
		row.SessionID = sid
		if err := a.store.AppendMessage(ctx, row); err != nil {
			return "", err
		}
	}
	a.setCurrentSession(sid)
	return sid, nil
}

// loadAgentPrompt reads an agent definition (frontmatter + body) as the
// system prompt. Returns "" when the definition is missing.
func (a *App) loadAgentPrompt(name string) (string, error) {
	body, _, _, err := a.loadAgentDef(name)
	return body, err
}

// loadAgentDef reads an agent definition (architecture 4.1.1): body is the
// system prompt; the frontmatter carries optional model and a permission
// subset (three-state rules, narrowed against the parent at dispatch).
func (a *App) loadAgentDef(name string) (body, model string, perms map[string]core.Effect, err error) {
	for _, root := range []string{
		filepath.Join(a.workDir, ".agents", "agents"),
		filepath.Join(a.home, "agents"),
	} {
		path := filepath.Join(root, name+".md")
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		fm, body, ferr := frontmatter.Split(data)
		if ferr != nil {
			continue
		}
		model, perms = parseAgentFrontmatter(fm)
		return body, model, perms, nil
	}
	return "", "", nil, fmt.Errorf("app: agent %q not found", name)
}

// parseAgentFrontmatter extracts model and the permission subset from an
// agent definition's YAML frontmatter. Unknown shapes are ignored: a
// malformed declaration narrows nothing rather than widening access.
func parseAgentFrontmatter(fm []byte) (string, map[string]core.Effect) {
	if len(fm) == 0 {
		return "", nil
	}
	var doc struct {
		Model      string            `yaml:"model"`
		Permission map[string]string `yaml:"permission"`
	}
	if err := yaml.Unmarshal(fm, &doc); err != nil {
		return "", nil
	}
	perms := map[string]core.Effect{}
	for action, eff := range doc.Permission {
		e := core.Effect(strings.ToLower(strings.TrimSpace(eff)))
		if e != core.EffectAllow && e != core.EffectAsk && e != core.EffectDeny {
			continue // never widen: unknown effect drops the rule
		}
		perms[action] = e
	}
	if len(perms) == 0 {
		perms = nil
	}
	return strings.TrimSpace(doc.Model), perms
}

// ------------------------------------------------------------ core seams

// AppendMessage persists one conversation message (core.MessageSink).
func (a *App) AppendMessage(_ context.Context, msg core.Message) error {
	id, err := newID("msg")
	if err != nil {
		return err
	}
	return a.store.AppendMessage(context.Background(), store.Message{
		ID:        id,
		SessionID: a.CurrentSession(),
		Role:      string(msg.Role),
		Content:   marshalMessage(msg),
		CreatedAt: msg.Timestamp,
	})
}

// ReadSession returns a session's transcript (core.SessionReader).
func (a *App) ReadSession(ctx context.Context, sessionID string, limit int) ([]core.Message, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := a.store.ListMessages(ctx, sessionID, limit, 0)
	if err != nil {
		return nil, err
	}
	out := make([]core.Message, 0, len(rows))
	for _, row := range rows {
		out = append(out, unmarshalMessage(row.Role, row.Content, row.CreatedAt))
	}
	hydrateImages(out)
	return out, nil
}

// hydrateImages reloads image bytes from their artifact pointers for a
// replayed session. Unreadable files keep Data nil; the request builder then
// substitutes a text note instead of silently dropping the turn's context.
func hydrateImages(msgs []core.Message) {
	for i := range msgs {
		for j := range msgs[i].Content {
			p := &msgs[i].Content[j]
			if p.Type != "image" || p.Image == nil || len(p.Image.Data) > 0 || p.Image.Path == "" {
				continue
			}
			data, err := os.ReadFile(p.Image.Path)
			if err != nil || len(data) > core.MaxImageBytes {
				continue
			}
			prepared, mime, err := core.PrepareImage(data)
			if err != nil {
				continue
			}
			p.Image.Data = prepared
			p.Image.MIME = mime
		}
	}
}

// ReadRecentSession returns up to the last limit messages of a session in
// append order (REPL session-switch replay).
func (a *App) ReadRecentSession(ctx context.Context, sessionID string, limit int) ([]core.Message, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := a.store.ListRecentMessages(ctx, sessionID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]core.Message, 0, len(rows))
	for _, row := range rows {
		out = append(out, unmarshalMessage(row.Role, row.Content, row.CreatedAt))
	}
	hydrateImages(out)
	return out, nil
}

// WriteHandoff persists a handoff note as a session attachment (KV) and
// mirrors decisions/corrections into project memory (core.HandoffWriter).
func (a *App) WriteHandoff(ctx context.Context, sessionID string, note core.HandoffNote) error {
	raw, err := json.Marshal(note)
	if err != nil {
		return err
	}
	if err := a.store.SetKV(ctx, "handoff:"+sessionID, raw); err != nil {
		return err
	}
	return nil
}

// SavePlan persists a session's finalized plan as a session KV attachment
// (core.PlanStore).
func (a *App) SavePlan(ctx context.Context, sessionID string, p core.Plan) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return a.store.SetKV(ctx, "plan:"+sessionID, raw)
}

// LoadPlan returns a session's finalized plan (core.PlanStore). A missing plan
// yields the zero Plan and no error: the executor just runs ReAct.
func (a *App) LoadPlan(ctx context.Context, sessionID string) (core.Plan, error) {
	raw, err := a.store.GetKV(ctx, "plan:"+sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return core.Plan{}, nil
		}
		return core.Plan{}, err
	}
	var p core.Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return core.Plan{}, err
	}
	return p, nil
}

// Mirror double-writes settled decisions and user corrections into long-term
// memory (core.MemoryMirror).
func (a *App) Mirror(ctx context.Context, kind, text string) error {
	_, err := a.memory.AddMemory(ctx, store.Memory{
		Kind:    "fact",
		Type:    "semantic",
		Name:    kind,
		Content: text,
		Source:  "explicit",
		Status:  "active",
	})
	return err
}

// -------------------------------------------------------- memory adapters

// Remember stores a long-term memory (core.MemoryStore).
func (a *App) Remember(ctx context.Context, kind, name string, keywords, triggers []string, content string) (string, error) {
	if !core.ValidKind(kind) {
		kind = core.KindFact
	}
	ctx, span := trace.Start(ctx, a.tracer, trace.KindMemoryWrite, "remember", map[string]any{
		"kind": kind, "triggers": len(triggers),
	})
	m, err := a.memory.AddMemory(ctx, store.Memory{
		Kind:     kind,
		Type:     lessonType(triggers),
		Name:     name,
		Keywords: keywords,
		Triggers: triggers,
		Content:  content,
		Source:   "explicit",
		Status:   "active",
	})
	if span != nil {
		span.SetAttr("memory_id", m.ID)
		span.SetAttr("domain", m.Domain)
		span.SetAttr("status", m.Status)
		span.SetAttr("source", m.Source)
		span.End(err)
	}
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

// Recall queries long-term memory by conversation context (core.MemoryStore).
// An empty kind searches all four kinds.
func (a *App) Recall(ctx context.Context, query, kind string, topK int) ([]core.MemoryEntry, error) {
	if topK <= 0 {
		topK = 5
	}
	// The query text is deliberately absent: it can contain user data. Hit ids
	// and ordering are enough to explain which memory shaped the turn.
	ctx, span := trace.Start(ctx, a.tracer, trace.KindMemoryRecall, "recall", map[string]any{
		"kind": kind, "top_k": topK,
	})
	hits, err := a.memory.Recall(ctx, query, a.runWorkDir(ctx), topK)
	out := make([]core.MemoryEntry, 0, len(hits))
	for _, h := range hits {
		if kind != "" && h.Kind != kind {
			continue
		}
		out = append(out, core.MemoryEntry{
			ID: h.ID, Name: h.Name, Kind: h.Kind, Keywords: h.Keywords,
			Triggers: h.Triggers, Content: h.Content, Status: h.Status,
		})
	}
	if span != nil {
		span.SetAttr("hits", len(hits))
		span.SetAttr("returned", len(out))
		span.SetAttr("memory_ids", memoryIDs(out))
		span.SetAttr("statuses", memoryStatuses(out))
		span.End(err)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// memoryIDs lists recalled memory ids (bounded to keep spans small).
func memoryIDs(entries []core.MemoryEntry) []string {
	out := make([]string, 0, len(entries))
	for i, e := range entries {
		if i == 10 {
			break
		}
		out = append(out, e.ID)
	}
	return out
}

func memoryStatuses(entries []core.MemoryEntry) []string {
	out := make([]string, 0, len(entries))
	for i, e := range entries {
		if i == 10 {
			break
		}
		out = append(out, e.Status)
	}
	return out
}

// lessonType marks entries carrying triggers as procedural lessons.
func lessonType(triggers []string) string {
	if len(triggers) > 0 {
		return "procedural"
	}
	return "semantic"
}

const (
	// lessonInjectLimit bounds how many trigger hits enter one user turn.
	lessonInjectLimit = 3
	// lessonBodyMax clips one lesson body (runes) to keep the input lean.
	lessonBodyMax = 400
)

// lessonsFor renders trigger-matched memories as a user-channel block. An
// empty result means nothing matched (the turn input is unchanged).
func (a *App) lessonsFor(ctx context.Context, input string) string {
	ctx, span := trace.Start(ctx, a.tracer, trace.KindMemoryRecall, "lesson_injection",
		map[string]any{"limit": lessonInjectLimit})
	matches, err := a.memory.TriggerMatches(ctx, input, a.runWorkDir(ctx), lessonInjectLimit)
	if err != nil || len(matches) == 0 {
		if span != nil {
			span.SetAttr("hits", 0)
			span.End(err)
		}
		return ""
	}
	var ids []string
	var truncated int
	for _, m := range matches {
		ids = append(ids, m.ID)
		if len([]rune(strings.TrimSpace(m.Content))) > lessonBodyMax {
			truncated++
		}
	}
	if span != nil {
		span.SetAttr("hits", len(matches))
		span.SetAttr("memory_ids", ids)
		span.SetAttr("statuses", lessonStatuses(matches))
		span.SetAttr("truncated", truncated)
		span.End(err)
	}
	var b strings.Builder
	b.WriteString("\n\n[关联经验 / lessons] 以下经验由触发词命中，执行前先核对：")
	for _, m := range matches {
		body := strings.TrimSpace(m.Content)
		if body == "" {
			body = m.Name
		}
		if r := []rune(body); len(r) > lessonBodyMax {
			body = string(r[:lessonBodyMax]) + "…"
		}
		fmt.Fprintf(&b, "\n- [%s] %s: %s", m.Kind, m.Name, body)
	}
	return b.String()
}

// lessonStatuses lists the status of injected lessons (active expected; a
// superseded hit would be a bug the trace makes visible).
func lessonStatuses(matches []store.Memory) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.Status)
	}
	return out
}

// ------------------------------------------------------------ session mgmt

// setCurrentSession records the run's session (mutex-guarded: concurrent runs
// touch this field).
func (a *App) setCurrentSession(id string) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	a.currentSession = id
}

// CurrentSession reports the most recently resolved session id.
func (a *App) CurrentSession() string {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	return a.currentSession
}

// resolveSession maps "" to a fresh session, "continue" to the most recent,
// and any other value to that existing session using process-wide settings.
func (a *App) resolveSession(ctx context.Context, sessionID string) (string, error) {
	_, agentModel, _, override := a.agentSettings()
	model := modelFor(a.cfg.LLM.Model, agentModel, override)
	return a.resolveSessionForRun(ctx, sessionID, "", model)
}

// EnsureSession returns the session a run will write to, creating one when the
// caller starts a fresh conversation. The daemon calls this before dispatch so
// POST /tasks can respond with the id and the client can navigate to the
// transcript immediately instead of waiting for the run to finish.
func (a *App) EnsureSession(ctx context.Context, sessionID, agentName, modelOverride string) (string, error) {
	_, agentModel, _, override := a.agentSettings()
	if agentName != "" {
		_, model, _, err := a.loadAgentDef(agentName)
		if err != nil {
			return "", err
		}
		agentModel = model
	}
	if modelOverride != "" {
		override = modelOverride
	}
	return a.resolveSessionForRun(ctx, sessionID, agentName, modelFor(a.cfg.LLM.Model, agentModel, override))
}

// resolveSessionForRun maps a requested id onto a stored session, creating a
// fresh one when the id is empty.
func (a *App) resolveSessionForRun(ctx context.Context, sessionID, agentName, model string) (string, error) {
	switch sessionID {
	case "":
		id, err := newID("ses")
		if err != nil {
			return "", err
		}
		if err := a.store.CreateSession(ctx, store.Session{
			ID: id, Agent: agentName, Model: model,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			return "", err
		}
		a.setCurrentSession(id)
		return id, nil
	case "continue":
		sessions, err := a.store.ListSessions(ctx, 1, 0)
		if err != nil {
			return "", err
		}
		if len(sessions) == 0 {
			return a.resolveSessionForRun(ctx, "", agentName, model)
		}
		a.setCurrentSession(sessions[0].ID)
		return sessions[0].ID, nil
	default:
		if _, err := a.store.GetSession(ctx, sessionID); err != nil {
			return "", fmt.Errorf("app: session %q: %w", sessionID, err)
		}
		a.setCurrentSession(sessionID)
		return sessionID, nil
	}
}

// loadHistory converts stored rows back into core messages.
func (a *App) loadHistory(ctx context.Context, sessionID string) ([]core.Message, error) {
	rows, err := a.store.ListMessages(ctx, sessionID, 1000, 0)
	if err != nil {
		return nil, err
	}
	out := make([]core.Message, 0, len(rows))
	for _, row := range rows {
		out = append(out, unmarshalMessage(row.Role, row.Content, row.CreatedAt))
	}
	hydrateImages(out)
	return out, nil
}

// addSessionUsage accumulates one turn's billing counters into the session row
// and bumps its activity timestamp.
func (a *App) addSessionUsage(ctx context.Context, sessionID string, ev core.ProgressEvent) error {
	s, err := a.store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	s.TokensIn += ev.TokensIn
	s.TokensOut += ev.TokensOut
	s.CacheReadTokens += ev.CacheReadTokens
	s.ReasoningTokens += ev.ReasoningTokens
	s.CostUSD += ev.CostUSD
	// The run reports the model it actually routed to (agent definition or
	// fallback provider may differ from the config default).
	if ev.Model != "" {
		s.Model = ev.Model
	}
	s.UpdatedAt = time.Now().UTC()
	return a.store.UpdateSession(ctx, s)
}

// touchSession updates the session's activity timestamp.
func (a *App) touchSession(ctx context.Context, sessionID string) error {
	s, err := a.store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	s.UpdatedAt = time.Now().UTC()
	return a.store.UpdateSession(ctx, s)
}

// titleSession names the session after the first turn: one cheap LLM call
// (reasoning off) over the opening exchange, ≤20 chars, stored as the title.
// Best-effort — on failure the title stays empty and listings fall back to
// the first user message. Later turns skip it (title already set).
func (a *App) titleSession(ctx context.Context, sessionID, userInput string, reply core.Message) {
	if a.backend == nil {
		return
	}
	s, err := a.store.GetSession(ctx, sessionID)
	if err != nil || strings.TrimSpace(s.Title) != "" {
		return
	}
	var replyText strings.Builder
	for _, p := range reply.Content {
		replyText.WriteString(p.Text)
	}
	prompt := fmt.Sprintf("Give this conversation a title of at most 20 characters (plain text, no quotes, use the conversation's language). Reply with the title only.\nUser: %s\nAssistant: %s",
		clampRunes(userInput, 600), clampRunes(replyText.String(), 600))
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := a.backend.Chat(tctx, core.Request{
		Model:           a.cfg.LLM.Model,
		SessionID:       sessionID,
		ReasoningEffort: "none", // a title never pays for thinking
		Messages:        []core.Message{{Role: core.RoleUser, Content: []core.Part{{Type: "text", Text: prompt}}}},
	})
	if err != nil {
		return
	}
	var sb strings.Builder
	for _, p := range resp.Message.Content {
		sb.WriteString(p.Text)
	}
	title := strings.Trim(sb.String(), " \t\n\"'“”‘’")
	if title == "" {
		return
	}
	s.Title = clampRunes(title, 20)
	_ = a.store.UpdateSession(ctx, s)
}

// clampRunes cuts s to n runes without an ellipsis (label budgets).
func clampRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// -------------------------------------------------------------- utilities

// msgBlob is the persisted shape of a core.Message. Content alone is not
// enough: tool_calls and tool_call_id must round-trip too, otherwise replayed
// history has orphaned tool results and compression can never fire.
type msgBlob struct {
	Content    []core.Part     `json:"content"`
	ToolCalls  []core.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

func marshalMessage(msg core.Message) []byte {
	raw, err := json.Marshal(msgBlob{
		Content:    truncateParts(msg.Content, store.MaxMessageBytes),
		ToolCalls:  msg.ToolCalls,
		ToolCallID: msg.ToolCallID,
	})
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// truncateParts caps the total text of a message's parts before marshalling.
// Structured truncation keeps the JSON blob valid: the store's byte-level cap
// is a last resort and would cut the blob mid-string, leaving content that no
// longer parses (breaks export and replay). Returns parts unchanged when
// under the cap.
func truncateParts(parts []core.Part, maxBytes int) []core.Part {
	total := 0
	for _, p := range parts {
		total += len(p.Text)
	}
	if total <= maxBytes {
		return parts
	}
	out := make([]core.Part, 0, len(parts))
	budget := maxBytes
	for _, p := range parts {
		if budget <= 0 {
			break
		}
		if len(p.Text) <= budget {
			out = append(out, p)
			budget -= len(p.Text)
			continue
		}
		// Cut this part at a rune boundary inside the budget.
		cut := budget
		for cut > 0 && !utf8.RuneStart(p.Text[cut]) {
			cut--
		}
		p.Text = p.Text[:cut] + "…[truncated]"
		out = append(out, p)
		break
	}
	return out
}

func unmarshalMessage(role string, raw []byte, at time.Time) core.Message {
	var blob msgBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		return core.Message{Role: core.Role(role), Timestamp: at}
	}
	return core.Message{
		Role:       core.Role(role),
		Content:    blob.Content,
		ToolCalls:  blob.ToolCalls,
		ToolCallID: blob.ToolCallID,
		Timestamp:  at,
	}
}

// newID generates a prefixed unique identifier for store rows.
func newID(prefix string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%016x", prefix, buf), nil
}
