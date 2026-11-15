// Package config loads ~/.chyan/config.toml and provides directory
// initialization plus first-run migration from the legacy OS config/cache
// directories into ~/.chyan.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Duration is a time.Duration that decodes from TOML duration strings
// like "30s". The stdlib type only accepts integer nanoseconds.
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(b), err)
	}
	*d = Duration(v)
	return nil
}

// D converts to time.Duration for consumers.
func (d Duration) D() time.Duration { return time.Duration(d) }

// LLM configures the model routing layer (A1). API keys are referenced
// via "secret:<name>", never stored here. Fallback endpoints form the FR-1
// fallback chain tried in order when the primary provider fails.
type LLM struct {
	BaseURL     string            `toml:"base_url"`
	APIKey      string            `toml:"api_key"` // "secret:<name>" or literal
	Model       string            `toml:"model"`
	Headers     map[string]string `toml:"headers"` // extra request headers; values may be "secret:<name>"
	Temperature float64           `toml:"temperature"`
	MaxTokens   int               `toml:"max_tokens"`
	// Pricing drives cost_usd accounting, in USD per 1M tokens (the unit
	// providers publish). Zero = unknown, which leaves cost at 0 instead of
	// inventing a number.
	CostPer1MIn     float64       `toml:"cost_per_1m_in"`
	CostPer1MOut    float64       `toml:"cost_per_1m_out"`
	CostPer1MCached float64       `toml:"cost_per_1m_cached"` // cached prompt rate; 0 = same as input
	Fallback        []LLMEndpoint `toml:"fallback"`           // [[llm.fallback]] backup endpoints
	// Vision is "auto" (send images and let the provider answer) or "off"
	// (reject image input up front with a readable error). FR-10.
	Vision string `toml:"vision"`
}

// LLMEndpoint is one backup provider-model pair in the fallback chain.
type LLMEndpoint struct {
	BaseURL         string            `toml:"base_url"`
	APIKey          string            `toml:"api_key"` // "secret:<name>" or literal
	Model           string            `toml:"model"`
	Headers         map[string]string `toml:"headers"`
	CostPer1MIn     float64           `toml:"cost_per_1m_in"`
	CostPer1MOut    float64           `toml:"cost_per_1m_out"`
	CostPer1MCached float64           `toml:"cost_per_1m_cached"`
}

// Context configures the window budget and compression thresholds.
// Window fields feed the FR-1 layered window resolution
// (config > online catalog > builtin table > fallback).
type Context struct {
	ContextLimit        any     `toml:"context_limit"`  // pin the window (int or numeric string); "" = auto
	OnlineCatalog       bool    `toml:"online_catalog"` // enable the models.dev layer
	WindowTable         string  `toml:"window_table"`   // custom {"windows": {...}} JSON path
	DefaultLimit        int     `toml:"default_limit"`  // layer-4 fallback window
	ReservedTokens      int     `toml:"reserved_tokens"`
	TriggerRatio        float64 `toml:"trigger_ratio"`
	TriggerBytes        int     `toml:"trigger_bytes"`
	CompressTargetRatio float64 `toml:"compress_target_ratio"`
	MaxRecentTurns      int     `toml:"max_recent_turns"`
	SummaryRatio        float64 `toml:"summary_ratio"`
	SummaryMaxRatio     float64 `toml:"summary_max_ratio"`
	FoldToolBytes       int     `toml:"fold_tool_bytes"` // older-turn tool outputs above this become head/tail digests (default 2048, <0 off)
	// LoadProjectInstructions injects AGENTS.md / CLAUDE.md / .cursor/rules
	// into the system prompt (D32). Default true; false keeps rules out.
	LoadProjectInstructions bool `toml:"load_project_instructions"`
}

// Permission is one three-state rule: effect for action on a resource
// glob. P2 ships only structure; the rule engine lands in P3.
type Permission struct {
	Action   string `toml:"action"`   // bash/edit/read/web/...
	Resource string `toml:"resource"` // glob pattern
	Effect   string `toml:"effect"`   // allow/ask/deny
}

// Review configures ask-effect adjudication (architecture 4.2).
type Review struct {
	Mode            string   `toml:"mode"`  // human|ai|full
	Model           string   `toml:"model"` // LLM fallback reviewer; empty = rules only
	EscalateTimeout Duration `toml:"escalate_timeout"`
}

// MCPServer is one external MCP server connection (architecture 4.8).
type MCPServer struct {
	Type             string            `toml:"type"` // remote|local
	URL              string            `toml:"url"`
	Command          []string          `toml:"command"`
	Headers          map[string]string `toml:"headers"`           // values may be "secret:<name>"
	CatalogTimeout   int               `toml:"catalog_timeout"`   // ms
	ExecutionTimeout int               `toml:"execution_timeout"` // ms
	Disabled         bool              `toml:"disabled"`
}

// Tools configures the built-in tool limits (architecture 4.1).
type Tools struct {
	ToolOutputMaxBytes int      `toml:"tool_output_max_bytes"`
	BashTimeout        Duration `toml:"bash_timeout"`
	MaxToolIterations  int      `toml:"max_tool_iterations"`
	MaxToolRelays      int      `toml:"max_tool_relays"` // reflexion relays after the iteration cap: 0 off, <0 unlimited
	FileMaxBytes       int64    `toml:"file_max_bytes"`  // read/write single-file cap
	// MaxRepeatedToolFailures blocks an identical tool call after this many
	// failures in one turn (0 = default 3, negative disables).
	MaxRepeatedToolFailures int `toml:"max_repeated_tool_failures"`
}

// Cache configures the tool intermediate-artifact directory TTL.
type Cache struct {
	TTLHours int `toml:"ttl_hours"` // files older than this are cleaned
}

// MCP holds the `[mcp.servers.<name>]` table.
type MCP struct {
	Servers map[string]MCPServer `toml:"servers"`
}

// Channel is one outbound IM channel under `[channels.<name>]`. Credential
// values may be "secret:<name>" (D20); receive_id_type matters for feishu.
type Channel struct {
	Type          string            `toml:"type"` // feishu | telegram
	Token         string            `toml:"token"`
	AppID         string            `toml:"app_id"`
	AppSecret     string            `toml:"app_secret"`
	WebhookToken  string            `toml:"webhook_token"`
	ReceiveIDType string            `toml:"receive_id_type"` // feishu: open_id|user_id|union_id|chat_id
	BaseURL       string            `toml:"base_url"`        // injectable for tests
	Headers       map[string]string `toml:"headers"`
	Disabled      bool              `toml:"disabled"`

	// Inbound trigger (FR-7): off by default. When enabled, at least one
	// whitelist entry is required; empty lists deny every message.
	Inbound         bool     `toml:"inbound"`
	AllowedChats    []string `toml:"allowed_chats"`
	AllowedUsers    []string `toml:"allowed_users"`
	MaxMessageChars int      `toml:"max_message_chars"`
	MinInterval     Duration `toml:"min_interval"`
	// MaxReplyChars caps outbound IM replies (0 = 800 runes); longer output is
	// truncated with a PWA link to the full session.
	MaxReplyChars int `toml:"max_reply_chars"`
	// IntentFilter only runs the agent for messages directed at it.
	IntentFilter bool `toml:"intent_filter"`
}

// Daemon holds the `[daemon]` limits for background dispatches. Zero values
// fall back to the appendix A defaults at startup.
type Daemon struct {
	MaxConcurrentTasks int      `toml:"max_concurrent_tasks"`
	TaskTimeout        Duration `toml:"task_timeout"`
}

// Config mirrors the config.toml layout (architecture 4.9).
type Config struct {
	LLM        LLM                `toml:"llm"`
	Context    Context            `toml:"context"`
	Permission []Permission       `toml:"permission"`
	Review     Review             `toml:"review"`
	MCP        MCP                `toml:"mcp"`
	Channels   map[string]Channel `toml:"channels"`
	Tools      Tools              `toml:"tools"`
	Cache      Cache              `toml:"cache"`
	Daemon     Daemon             `toml:"daemon"`
}

// Default returns the appendix A baseline configuration.
func Default() Config {
	return Config{
		LLM: LLM{
			Temperature: 0.1,
			MaxTokens:   4096,
			Vision:      "auto",
		},
		Context: Context{
			ReservedTokens:          4096,
			TriggerRatio:            0.7,
			TriggerBytes:            204800,
			CompressTargetRatio:     0.8,
			MaxRecentTurns:          4,
			SummaryRatio:            0.3,
			SummaryMaxRatio:         0.15,
			LoadProjectInstructions: true,
		},
		Review: Review{
			Mode:            "ai",
			EscalateTimeout: Duration(60 * time.Second),
		},
		MCP:      MCP{Servers: map[string]MCPServer{}},
		Channels: map[string]Channel{},
		Tools: Tools{
			ToolOutputMaxBytes:      20000,
			BashTimeout:             Duration(30 * time.Second),
			MaxToolIterations:       25,
			MaxToolRelays:           3,
			FileMaxBytes:            10 << 20, // 10MB
			MaxRepeatedToolFailures: 3,
		},
		Cache: Cache{TTLHours: 24},
		Daemon: Daemon{
			MaxConcurrentTasks: 4,
			TaskTimeout:        Duration(30 * time.Minute),
		},
	}
}

// Load reads config.toml and merges it over the defaults. A missing file
// yields pure defaults; a malformed file is an error. go-toml only
// overwrites fields present in the document, so absent fields keep
// defaults and declared mcp servers merge into the default map.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return Default(), fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}
