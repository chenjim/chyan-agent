package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDefaultMatchesAppendixA(t *testing.T) {
	c := Default()
	checks := map[string]struct{ got, want any }{
		"trigger_ratio":       {c.Context.TriggerRatio, 0.7},
		"trigger_bytes":       {c.Context.TriggerBytes, 204800},
		"compress_target":     {c.Context.CompressTargetRatio, 0.8},
		"max_recent_turns":    {c.Context.MaxRecentTurns, 4},
		"reserved_tokens":     {c.Context.ReservedTokens, 4096},
		"tool_output_max":     {c.Tools.ToolOutputMaxBytes, 20000},
		"max_tool_iterations": {c.Tools.MaxToolIterations, 25},
		"max_tool_relays":     {c.Tools.MaxToolRelays, 3},
		"file_max_bytes":      {c.Tools.FileMaxBytes, int64(10 << 20)},
		"review_mode":         {c.Review.Mode, "ai"},
		"cache_ttl_hours":     {c.Cache.TTLHours, 24},
		"bash_timeout":        {c.Tools.BashTimeout.D(), 30 * time.Second},
		"escalate_timeout":    {c.Review.EscalateTimeout.D(), 60 * time.Second},
		"llm_temperature":     {c.LLM.Temperature, 0.1},
		"llm_max_tokens":      {c.LLM.MaxTokens, 4096},
	}
	for name, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s = %v, want %v", name, ck.got, ck.want)
		}
	}
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("want pure defaults, got %+v", cfg)
	}
}

func TestLoadOverridesAndMerges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[llm]
base_url = "https://api.example/v1"
api_key = "secret:llm_key"
model = "test-model"

[context]
trigger_ratio = 0.9

[[permission]]
action = "bash"
resource = "rm *"
effect = "deny"

[review]
mode = "human"
escalate_timeout = "90s"

[mcp.servers.search]
type = "remote"
url = "http://127.0.0.1:17492/mcp"
[mcp.servers.search.headers]
Authorization = "secret:search_token"

[mcp.servers.playwright]
type = "local"
command = ["playwright-mcp", "--headless"]
catalog_timeout = 30000
execution_timeout = 30000

[channels.feishu]
type = "feishu"
app_id = "cli_x"
app_secret = "secret:feishu_secret"
receive_id_type = "chat_id"

[channels.tg]
type = "telegram"
token = "secret:tg_token"
disabled = true

[tools]
bash_timeout = "45s"
max_tool_iterations = 10

[cache]
ttl_hours = 48
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.LLM.BaseURL != "https://api.example/v1" || cfg.LLM.APIKey != "secret:llm_key" {
		t.Errorf("llm override lost: %+v", cfg.LLM)
	}
	if cfg.Context.TriggerRatio != 0.9 {
		t.Errorf("trigger_ratio = %v", cfg.Context.TriggerRatio)
	}
	if cfg.Context.TriggerBytes != 204800 { // untouched field keeps default
		t.Errorf("trigger_bytes = %v", cfg.Context.TriggerBytes)
	}
	if len(cfg.Permission) != 1 || cfg.Permission[0].Effect != "deny" {
		t.Errorf("permission = %+v", cfg.Permission)
	}
	if cfg.Review.Mode != "human" || cfg.Review.EscalateTimeout.D() != 90*time.Second {
		t.Errorf("review = %+v", cfg.Review)
	}
	if cfg.Review.Model != "" { // default stays empty
		t.Errorf("review.model = %q", cfg.Review.Model)
	}
	if len(cfg.MCP.Servers) != 2 {
		t.Fatalf("mcp servers = %+v", cfg.MCP.Servers)
	}
	if cfg.MCP.Servers["search"].Headers["Authorization"] != "secret:search_token" {
		t.Errorf("mcp header = %+v", cfg.MCP.Servers["search"])
	}
	if got := cfg.MCP.Servers["playwright"].Command; len(got) != 2 || got[0] != "playwright-mcp" {
		t.Errorf("local command = %v", got)
	}
	if len(cfg.Channels) != 2 {
		t.Fatalf("channels = %+v", cfg.Channels)
	}
	if fs := cfg.Channels["feishu"]; fs.Type != "feishu" || fs.AppSecret != "secret:feishu_secret" || fs.ReceiveIDType != "chat_id" {
		t.Errorf("feishu channel = %+v", fs)
	}
	if tg := cfg.Channels["tg"]; tg.Type != "telegram" || !tg.Disabled {
		t.Errorf("tg channel = %+v", tg)
	}
	if cfg.Tools.BashTimeout.D() != 45*time.Second {
		t.Errorf("bash_timeout = %v", cfg.Tools.BashTimeout.D())
	}
	if cfg.Tools.ToolOutputMaxBytes != 20000 { // untouched field keeps default
		t.Errorf("tool_output_max_bytes = %d", cfg.Tools.ToolOutputMaxBytes)
	}
	if cfg.Cache.TTLHours != 48 {
		t.Errorf("ttl_hours = %d", cfg.Cache.TTLHours)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, body string
	}{
		{"malformed toml", "[llm\nbase_url ="},
		{"bad duration", `[tools]
bash_timeout = "soon"`},
		{"wrong type", `[context]
trigger_ratio = "high"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err == nil {
				t.Fatal("want error")
			}
			if !reflect.DeepEqual(cfg, Default()) {
				t.Errorf("want defaults on error, got %+v", cfg)
			}
		})
	}
}

func TestDurationUnmarshalText(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("1m30s")); err != nil {
		t.Fatal(err)
	}
	if d.D() != 90*time.Second {
		t.Errorf("got %v", d.D())
	}
	if err := d.UnmarshalText([]byte("xx")); err == nil {
		t.Error("want error for invalid duration")
	}
}

func TestLoadReadError(t *testing.T) {
	// pointing at a directory makes ReadFile fail with a non-not-exist error
	cfg, err := Load(t.TempDir())
	if err == nil {
		t.Fatal("want read error")
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("want defaults on error, got %+v", cfg)
	}
}

// TestLoadLLMFallbackChain parses [[llm.fallback]] entries and the [context]
// window resolution fields (FR-1).
func TestLoadLLMFallbackChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[llm]
base_url = "https://primary.example/v1"
model = "main-model"

[[llm.fallback]]
base_url = "https://backup.example/v1"
api_key = "secret:backup_key"
model = "backup-model"

[context]
context_limit = 200000
online_catalog = true
window_table = "/etc/chyan/windows.json"
default_limit = 65536
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.LLM.Fallback) != 1 {
		t.Fatalf("fallback entries = %+v", cfg.LLM.Fallback)
	}
	fb := cfg.LLM.Fallback[0]
	if fb.BaseURL != "https://backup.example/v1" || fb.Model != "backup-model" || fb.APIKey != "secret:backup_key" {
		t.Errorf("fallback entry = %+v", fb)
	}
	// TOML decodes integers into `any` as int64; the resolver accepts both.
	if v, ok := cfg.Context.ContextLimit.(int64); !ok || v != 200000 {
		t.Errorf("context_limit = %v (%T)", cfg.Context.ContextLimit, cfg.Context.ContextLimit)
	}
	if !cfg.Context.OnlineCatalog || cfg.Context.WindowTable != "/etc/chyan/windows.json" || cfg.Context.DefaultLimit != 65536 {
		t.Errorf("window fields = %+v", cfg.Context)
	}
}

// TestLoadLLMHeadersAndPricing parses per-endpoint request headers and the
// per-1M-token prices that drive cost_usd accounting.
func TestLoadLLMHeadersAndPricing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[llm]
base_url = "https://gateway.example/v1"
model = "main-model"
cost_per_1m_in = 2.8
cost_per_1m_out = 4.2
cost_per_1m_cached = 0.28
[llm.headers]
"x-opencode-session" = "sess-1"
Authorization = "secret:gateway_key"

[[llm.fallback]]
base_url = "https://backup.example/v1"
model = "backup-model"
cost_per_1m_in = 0.5
[llm.fallback.headers]
"x-route" = "backup"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.LLM.Headers["x-opencode-session"]; got != "sess-1" {
		t.Errorf("primary header = %q", got)
	}
	if got := cfg.LLM.Headers["Authorization"]; got != "secret:gateway_key" {
		t.Errorf("primary auth header = %q", got)
	}
	if cfg.LLM.CostPer1MIn != 2.8 || cfg.LLM.CostPer1MOut != 4.2 || cfg.LLM.CostPer1MCached != 0.28 {
		t.Errorf("primary pricing = %+v", cfg.LLM)
	}
	fb := cfg.LLM.Fallback[0]
	if fb.Headers["x-route"] != "backup" || fb.CostPer1MIn != 0.5 {
		t.Errorf("fallback headers/pricing = %+v", fb)
	}
}

func TestDaemonDefaults(t *testing.T) {
	c := Default()
	if c.Daemon.MaxConcurrentTasks != 4 {
		t.Errorf("max_concurrent_tasks = %d, want 4", c.Daemon.MaxConcurrentTasks)
	}
	if got := c.Daemon.TaskTimeout.D(); got != 30*time.Minute {
		t.Errorf("task_timeout = %v, want 30m", got)
	}
}

func TestDaemonOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[daemon]\nmax_concurrent_tasks = 2\ntask_timeout = \"5m\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Daemon.MaxConcurrentTasks != 2 {
		t.Errorf("max_concurrent_tasks = %d, want 2", cfg.Daemon.MaxConcurrentTasks)
	}
	if got := cfg.Daemon.TaskTimeout.D(); got != 5*time.Minute {
		t.Errorf("task_timeout = %v, want 5m", got)
	}
}

func TestProjectInstructionsDefaultAndOff(t *testing.T) {
	if !Default().Context.LoadProjectInstructions {
		t.Fatal("project instructions should load by default")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[context]\nload_project_instructions = false\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Context.LoadProjectInstructions {
		t.Fatal("explicit false should disable instruction loading")
	}
}

func TestChannelInboundConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `[channels.feishu]
type = "feishu"
inbound = true
allowed_chats = ["oc_1"]
allowed_users = ["ou_1"]
max_message_chars = 500
min_interval = "1s"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ch := cfg.Channels["feishu"]
	if !ch.Inbound || len(ch.AllowedChats) != 1 || len(ch.AllowedUsers) != 1 {
		t.Fatalf("channel = %+v", ch)
	}
	if ch.MaxMessageChars != 500 || ch.MinInterval.D() != time.Second {
		t.Fatalf("limits = %d, %v", ch.MaxMessageChars, ch.MinInterval.D())
	}
}
