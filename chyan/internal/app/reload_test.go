// Reload (/reload) tests: the reloadable subset is re-read, failures keep the
// running configuration, and the reloaded rules actually bind tool calls (U3).
package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

// writeConfig rewrites the app's config.toml (reload source).
func writeConfig(t *testing.T, a *App, body string) {
	t.Helper()
	if err := os.WriteFile(a.cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReloadAppliesReviewModeAndRules(t *testing.T) {
	a, _ := newGatedApp(t, "[review]\nmode = \"human\"\n", nil)
	if got := a.ReviewMode(); got != core.ReviewHuman {
		t.Fatalf("start mode = %q", got)
	}
	writeConfig(t, a, "[review]\nmode = \"ai\"\n"+
		"[[permission]]\naction = \"bash\"\nresource = \"rm *\"\neffect = \"deny\"\n")
	res, err := a.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if res.ReviewMode != string(core.ReviewAI) || res.PermRules != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got := a.ReviewMode(); got != core.ReviewAI {
		t.Fatalf("mode after reload = %q, want ai", got)
	}
	// The reloaded deny rule must bind on the rebuilt registry.
	tool, ok := a.Registry().Get("bash")
	if !ok {
		t.Fatal("bash tool missing after reload")
	}
	r := tool.Execute(context.Background(), core.ToolCall{Input: map[string]any{"command": "rm -rf /tmp/x"}})
	if r.Err == nil || !strings.Contains(r.Err.Error(), "denied") {
		t.Fatalf("reloaded deny rule not applied: %v", r.Err)
	}
}

func TestReloadKeepsRunningSetupOnBadConfig(t *testing.T) {
	a, _ := newGatedApp(t, "[review]\nmode = \"human\"\n", nil)
	writeConfig(t, a, "[review\nmode = \"ai\"\n") // malformed TOML
	if _, err := a.Reload(); err == nil {
		t.Fatal("malformed config must fail")
	}
	if got := a.ReviewMode(); got != core.ReviewHuman {
		t.Fatalf("mode changed after failed reload: %q", got)
	}
	// A config without [llm].base_url cannot build a backend either; the
	// injected test backend is kept, so the failure comes from the parse above.
	writeConfig(t, a, "[review]\nmode = \"full\"\n")
	if _, err := a.Reload(); err != nil {
		t.Fatalf("valid config must reload: %v", err)
	}
	if got := a.ReviewMode(); got != core.ReviewFull {
		t.Fatalf("mode = %q, want full", got)
	}
}

func TestReloadRejectsUnusableLLM(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.toml")
	body := "[llm]\nbase_url = \"http://127.0.0.1:1/v1\"\nmodel = \"m\"\napi_key = \"\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{
		SkipExtensions: true, Home: home, MemoryRoot: t.TempDir(),
		WorkDir: t.TempDir(), ConfigPath: cfgPath, Store: store.NewMemory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	// Emptying base_url makes rebuild fail: the old backend must survive.
	if err := os.WriteFile(cfgPath, []byte("[llm]\nbase_url = \"\"\nmodel = \"m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reload(); err == nil {
		t.Fatal("reload without base_url must fail")
	}
	if a.backend == nil {
		t.Fatal("failed reload dropped the backend")
	}
}

func TestReloadRebuildsToolLimits(t *testing.T) {
	a, _ := newGatedApp(t, "[tools]\ntool_output_max_bytes = 111\n", nil)
	if got := core.ToolOutputMaxBytes(); got != 111 {
		t.Fatalf("start cap = %d", got)
	}
	writeConfig(t, a, "[tools]\ntool_output_max_bytes = 222\nbash_timeout = \"5s\"\n")
	res, err := a.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := core.ToolOutputMaxBytes(); got != 222 {
		t.Fatalf("cap after reload = %d, want 222", got)
	}
	if res.Tools == 0 {
		t.Fatal("tool count must be reported")
	}
	// The rebuilt registry still holds every built-in.
	for _, name := range []string{"read", "write", "bash"} {
		if _, ok := a.Registry().Get(name); !ok {
			t.Errorf("tool %q missing after reload", name)
		}
	}
}

func TestReloadReportsConfigPathAndModel(t *testing.T) {
	a, _ := newGatedApp(t, "[review]\nmode = \"ai\"\n", nil)
	writeConfig(t, a, "[review]\nmode = \"ai\"\n[llm]\nmodel = \"reload-model\"\n")
	res, err := a.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if res.ConfigPath != a.cfgPath || res.Model != "reload-model" {
		t.Fatalf("result = %+v", res)
	}
}

// TestReloadRefusesWhileRunInFlight: a turn holds runMu for its whole life, so
// reload must refuse (ErrBusy) instead of waiting or half-swapping; once the
// turn ends the same reload succeeds.
func TestReloadRefusesWhileRunInFlight(t *testing.T) {
	a, _ := newGatedApp(t, "[review]\nmode = \"ai\"\n", nil)
	writeConfig(t, a, "[review]\nmode = \"human\"\n")
	a.runMu.RLock() // simulate an in-flight turn
	_, err := a.Reload()
	a.runMu.RUnlock()
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if a.ReviewMode() == core.ReviewHuman {
		t.Fatal("refused reload must not apply the new config")
	}
	if _, err := a.Reload(); err != nil {
		t.Fatalf("reload after the turn: %v", err)
	}
	if a.ReviewMode() != core.ReviewHuman {
		t.Fatalf("mode = %q, want human", a.ReviewMode())
	}
}
