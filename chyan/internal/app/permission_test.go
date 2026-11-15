// Permission engine wiring tests: rule mapping, gate assembly and the real
// review-gate e2e paths (D23/D24).
package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/config"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

func TestBuildCheckerMapping(t *testing.T) {
	cfg := config.Config{Permission: []config.Permission{
		{Action: "bash", Resource: "rm *", Effect: "deny"},
		{Action: "write", Resource: "docs/**", Effect: "allow"},
		{Action: "edit", Resource: "y", Effect: "bogus"}, // dropped: never widens
	}}
	p := buildChecker(cfg)
	if got := p.Check("bash", "rm -rf /"); got != core.EffectDeny {
		t.Errorf("bash rm = %v, want deny", got)
	}
	if got := p.Check("write", "docs/a.md"); got != core.EffectAllow {
		t.Errorf("write docs = %v, want allow", got)
	}
	if got := p.Check("edit", "y"); got != core.EffectAsk {
		t.Errorf("invalid effect must drop the rule: edit y = %v, want ask", got)
	}
}

func TestReviewModeParsing(t *testing.T) {
	cases := []struct {
		mode string
		auto bool
		want core.ReviewMode
	}{
		{"human", false, core.ReviewHuman},
		{"full", false, core.ReviewFull},
		{"ai", false, core.ReviewAI},
		{"", false, core.ReviewAI},
		{"mystery", false, core.ReviewAI}, // unknown falls back to ai
		{"human", true, core.ReviewFull},  // --auto forces full
	}
	for _, c := range cases {
		cfg := config.Config{Review: config.Review{Mode: c.mode}}
		if got := reviewMode(cfg, c.auto); got != c.want {
			t.Errorf("reviewMode(%q, auto=%v) = %v, want %v", c.mode, c.auto, got, c.want)
		}
	}
}

// TestAppReviewModeForStatus: the daemon reports App.ReviewMode(), so the
// effective mode (including --auto) must be what the config resolves to.
func TestAppReviewModeForStatus(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		auto bool
		want core.ReviewMode
	}{
		{"default ai", "", false, core.ReviewAI},
		{"human from config", "[review]\nmode = \"human\"\n", false, core.ReviewHuman},
		{"auto forces full", "[review]\nmode = \"human\"\n", true, core.ReviewFull},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _ := newGatedApp(t, c.cfg, func(o *Options) { o.Auto = c.auto })
			if got := a.ReviewMode(); got != c.want {
				t.Errorf("ReviewMode() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestBuildGateNeedsBackendForReviewModel(t *testing.T) {
	cfg := config.Config{Review: config.Review{Model: "judge"}}
	if _, err := buildGate(cfg, Options{}); err == nil {
		t.Error("review.model without a backend must fail")
	}
	g, err := buildGate(cfg, Options{Backend: &stubBackend{}})
	if err != nil {
		t.Fatalf("buildGate: %v", err)
	}
	if g == nil {
		t.Fatal("buildGate must return a gate")
	}
}

// TestBuildGateDefaultsToMainModel: without [review].model the reviewer falls
// back to the main model with reasoning disabled (cheap one-word verdicts).
func TestBuildGateDefaultsToMainModel(t *testing.T) {
	cfg := config.Config{LLM: config.LLM{Model: "deepseek-v4.1-flash"}}
	g, err := buildGate(cfg, Options{Backend: &stubBackend{}})
	if err != nil {
		t.Fatalf("buildGate: %v", err)
	}
	rtg, ok := g.(*reviewToolGate)
	if !ok {
		t.Fatalf("gate type = %T", g)
	}
	rev, ok := rtg.gate.Reviewer().(*llmReviewer)
	if !ok {
		t.Fatalf("reviewer type = %T (main-model fallback missing)", rtg.gate.Reviewer())
	}
	if rev.model != "deepseek-v4.1-flash" {
		t.Errorf("reviewer model = %q, want main model", rev.model)
	}
	if rev.reasoningEffort != "none" {
		t.Errorf("reasoningEffort = %q, want none", rev.reasoningEffort)
	}
}

// TestLLMReviewerSendsReasoningEffort: the review request must carry
// reasoning_effort so reasoning models skip thinking for one-word verdicts.
func TestLLMReviewerSendsReasoningEffort(t *testing.T) {
	b := &stubBackend{responses: []core.Response{textResp("low")}}
	rev := &llmReviewer{backend: b, model: "m", reasoningEffort: "none"}
	if _, err := rev.Review(context.Background(), "bash", "ls", ""); err != nil {
		t.Fatal(err)
	}
	if b.lastRequest.ReasoningEffort != "none" {
		t.Errorf("request ReasoningEffort = %q, want none", b.lastRequest.ReasoningEffort)
	}
}

// stubReviewer is an injected core.RiskReviewer for gate tests.
type stubReviewer struct {
	v    core.RiskVerdict
	err  error
	seen []string
}

func (r *stubReviewer) Review(_ context.Context, action, resource, _ string) (core.RiskVerdict, error) {
	r.seen = append(r.seen, action+":"+resource)
	return r.v, r.err
}

// stubApprover is a scripted core.Approver.
type stubApprover struct {
	allow bool
	err   error
	reqs  []core.ApprovalRequest
}

func (a *stubApprover) Approve(_ context.Context, req core.ApprovalRequest) (bool, error) {
	a.reqs = append(a.reqs, req)
	return a.allow, a.err
}

// newGatedApp wires the full stack with the production gate assembled from
// cfgBody (config.toml text); mutate adjusts Options before New.
func newGatedApp(t *testing.T, cfgBody string, mutate func(*Options)) (*App, string) {
	t.Helper()
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	opts := Options{
		SkipExtensions: true,
		Home:           home,
		MemoryRoot:     t.TempDir(),
		WorkDir:        workDir,
		ConfigPath:     cfgPath,
		Backend:        &stubBackend{},
		Store:          store.NewMemory(),
	}
	if mutate != nil {
		mutate(&opts)
	}
	a, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, workDir
}

// auditEntries lists the app's audit trail for assertions.
func auditEntries(t *testing.T, a *App) []store.AuditEntry {
	t.Helper()
	entries, err := a.store.ListAudit(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	return entries
}

// Real-engine e2e: the assembled gate must deny a destructive bash command
// via the heuristic (high) and record it in the audit trail.
func TestGateEngineRejectsDangerousBash(t *testing.T) {
	a, _ := newGatedApp(t, "[review]\nmode = \"ai\"\n", nil)
	tool, _ := a.Registry().Get("bash")
	res := tool.Execute(context.Background(), core.ToolCall{
		Input: map[string]any{"command": "rm -rf /"},
	})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "denied") {
		t.Fatalf("dangerous bash must be denied, got %v", res.Err)
	}
	entries := auditEntries(t, a)
	if len(entries) == 0 {
		t.Fatal("denial must be audited")
	}
	e := entries[0]
	if e.Action != "bash" || e.Approved || e.Verdict != "high" {
		t.Errorf("audit = %+v", e)
	}
}

// D24: an uncertain ask with no reachable human conservatively denies and
// the file is never written.
func TestGateEngineWriteNoHumanDenies(t *testing.T) {
	a, workDir := newGatedApp(t, "[review]\nmode = \"ai\"\n", nil)
	tool, _ := a.Registry().Get("write")
	res := tool.Execute(context.Background(), core.ToolCall{
		Input: map[string]any{"path": "note.txt", "content": "hello"},
	})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "denied") {
		t.Fatalf("unreviewed write must deny, got %v", res.Err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "note.txt")); !os.IsNotExist(err) {
		t.Error("denied write must not touch the file")
	}
}

// Human approval path: the approver sees the request and the file is really
// written after approval.
func TestGateEngineWriteApproverAllows(t *testing.T) {
	ap := &stubApprover{allow: true}
	a, workDir := newGatedApp(t, "[review]\nmode = \"ai\"\n", func(o *Options) { o.Approver = ap })
	tool, _ := a.Registry().Get("write")
	res := tool.Execute(context.Background(), core.ToolCall{
		Input: map[string]any{"path": "ok.txt", "content": "hello"},
	})
	if res.Err != nil {
		t.Fatalf("approved write failed: %v", res.Err)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "ok.txt"))
	if err != nil || string(data) != "hello" {
		t.Errorf("written file = %q, %v", data, err)
	}
	if len(ap.reqs) != 1 || ap.reqs[0].Action != "write" {
		t.Errorf("approver requests = %+v", ap.reqs)
	}
}

// Explicit deny rules hard-block in every review mode.
func TestGateEngineExplicitDenyHardBlocks(t *testing.T) {
	for _, mode := range []string{"human", "ai", "full"} {
		cfg := "[review]\nmode = \"" + mode + "\"\n" +
			"[[permission]]\naction = \"bash\"\nresource = \"*\"\neffect = \"deny\"\n"
		a, _ := newGatedApp(t, cfg, nil)
		tool, _ := a.Registry().Get("bash")
		res := tool.Execute(context.Background(), core.ToolCall{
			Input: map[string]any{"command": "echo hi"},
		})
		if res.Err == nil || !strings.Contains(res.Err.Error(), "denied") {
			t.Errorf("mode %s: explicit deny must block, got %v", mode, res.Err)
		}
	}
}

// Config deny rules must bind read-only tools too: those skip the gate, so
// the wrapper has to consult the checker itself or the rule is bypassed.
func TestGateEngineConfigDenyBlocksReadOnlyTool(t *testing.T) {
	cfg := "[review]\nmode = \"ai\"\n" +
		"[[permission]]\naction = \"read\"\nresource = \"**\"\neffect = \"deny\"\n"
	a, workDir := newGatedApp(t, cfg, nil)
	path := filepath.Join(workDir, "secret.txt")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool, _ := a.Registry().Get("read")
	res := tool.Execute(context.Background(), core.ToolCall{
		Input: map[string]any{"path": "secret.txt"},
	})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "denied") {
		t.Fatalf("config deny must block read, got %v", res.Err)
	}
	if strings.Contains(res.Content, "data") {
		t.Error("denied read must not return file content")
	}
}

// An ask rule on a read-only tool forces the gate; full mode then approves.
func TestGateEngineConfigAskRoutesReadOnlyThroughGate(t *testing.T) {
	gate := &stubGate{approved: true, verdict: "low"}
	cfg := "[review]\nmode = \"ai\"\n" +
		"[[permission]]\naction = \"read\"\nresource = \"**\"\neffect = \"ask\"\n"
	a, workDir := newGatedApp(t, cfg, func(o *Options) { o.Reviewer = &stubReviewer{v: core.RiskLow} })
	a.SetGate(gate)
	path := filepath.Join(workDir, "note.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool, _ := a.Registry().Get("read")
	if res := tool.Execute(context.Background(), core.ToolCall{
		Input: map[string]any{"path": "note.txt"},
	}); res.Err != nil {
		t.Fatalf("approved read must succeed: %v", res.Err)
	}
	if len(gate.asks) != 1 || gate.asks[0].Action != "read" {
		t.Errorf("gate asks = %+v", gate.asks)
	}
}

// --auto forces full mode: uncertain asks auto-approve.
func TestGateEngineAutoFullApproves(t *testing.T) {
	a, _ := newGatedApp(t, "[review]\nmode = \"human\"\n", func(o *Options) { o.Auto = true })
	tool, _ := a.Registry().Get("bash")
	res := tool.Execute(context.Background(), core.ToolCall{
		Input: map[string]any{"command": "mkdir auto-dir"},
	})
	if res.Err != nil {
		t.Fatalf("auto mode must approve uncertain asks: %v", res.Err)
	}
}

// An unknown review mode falls back to ai behaviour: uncertain ask + no
// human denies.
func TestGateEngineUnknownModeFallsBackToAI(t *testing.T) {
	a, _ := newGatedApp(t, "[review]\nmode = \"mystery\"\n", nil)
	tool, _ := a.Registry().Get("bash")
	res := tool.Execute(context.Background(), core.ToolCall{
		Input: map[string]any{"command": "mkdir m-dir"},
	})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "denied") {
		t.Errorf("unknown mode must behave like ai (deny uncertain), got %v", res.Err)
	}
}

// The injected reviewer judges heuristic-uncertain asks in ai mode.
func TestGateEngineReviewerDecides(t *testing.T) {
	rev := &stubReviewer{v: core.RiskLow}
	a, workDir := newGatedApp(t, "[review]\nmode = \"ai\"\n", func(o *Options) { o.Reviewer = rev })
	tool, _ := a.Registry().Get("write")
	res := tool.Execute(context.Background(), core.ToolCall{
		Input: map[string]any{"path": "rev.txt", "content": "x"},
	})
	if res.Err != nil {
		t.Fatalf("reviewer low must approve: %v", res.Err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "rev.txt")); err != nil {
		t.Errorf("file must be written: %v", err)
	}
	if len(rev.seen) != 1 || !strings.HasPrefix(rev.seen[0], "write:") {
		t.Errorf("reviewer calls = %v", rev.seen)
	}
}

// The edit preview must carry a unified diff to the approver (preview only,
// nothing is written before approval).
func TestEditPreviewDiff(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "note.txt")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gate := &stubGate{approved: false, verdict: "high"}
	a := newTestApp(t, &stubBackend{})
	a.workDir = workDir
	a.SetGate(gate)

	tool, _ := a.Registry().Get("edit")
	res := tool.Execute(context.Background(), core.ToolCall{
		Input: map[string]any{"path": "note.txt", "old_string": "world", "new_string": "there"},
	})
	if res.Err == nil {
		t.Fatal("denied edit must error")
	}
	if len(gate.asks) != 1 {
		t.Fatalf("gate asks = %+v", gate.asks)
	}
	diff := gate.asks[0].Diff
	for _, want := range []string{"--- a/note.txt", "+++ b/note.txt", "-hello world", "+hello there"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing %q:\n%s", want, diff)
		}
	}
	// Preview only: the file is untouched after a denied edit.
	data, _ := os.ReadFile(path)
	if string(data) != "hello world\n" {
		t.Errorf("file changed before approval: %q", data)
	}
}

func TestPreviewBytesVariants(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "f.txt")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// write: replace / append.
	_, after := previewBytes(workDir, "write", core.ToolCall{
		Input: map[string]any{"path": "f.txt", "content": "x"}})
	if string(after) != "x" {
		t.Errorf("write after = %q", after)
	}
	_, after = previewBytes(workDir, "write", core.ToolCall{
		Input: map[string]any{"path": "f.txt", "content": "x", "mode": "append"}})
	if string(after) != "a\nb\nx" {
		t.Errorf("append after = %q", after)
	}
	// delete: after is empty.
	before, after := previewBytes(workDir, "delete", core.ToolCall{
		Input: map[string]any{"path": "f.txt"}})
	if string(before) != "a\nb\n" || after != nil {
		t.Errorf("delete before/after = %q/%q", before, after)
	}
	// missing file reads as empty before.
	before, _ = previewBytes(workDir, "write", core.ToolCall{
		Input: map[string]any{"path": "ghost.txt", "content": "x"}})
	if before != nil {
		t.Errorf("missing file before = %q", before)
	}
}

func TestApplyEditPreview(t *testing.T) {
	cases := []struct {
		name string
		text string
		in   map[string]any
		want string
	}{
		{"single", "abc", map[string]any{"old_string": "b", "new_string": "X"}, "aXc"},
		{"replaceAll", "x x", map[string]any{"old_string": "x", "new_string": "y", "replace_all": true}, "y y"},
		{"ambiguous", "x x", map[string]any{"old_string": "x", "new_string": "y"}, "x x"},
		{"notFound", "abc", map[string]any{"old_string": "zz", "new_string": "y"}, "abc"},
		{"emptyOld", "abc", map[string]any{"new_string": "y"}, "abc"},
	}
	for _, c := range cases {
		in := c.in
		got := applyEditPreview(c.text, in)
		if got != c.want {
			t.Errorf("%s = %q, want %q", c.name, got, c.want)
		}
	}
	// replace_all with ambiguous match rewrites everywhere.
	if got := applyEditPreview("x x", map[string]any{
		"old_string": "x", "new_string": "y", "replace_all": true}); got != "y y" {
		t.Errorf("replaceAll ambiguous = %q", got)
	}
}

func TestPreviewOfNonFileActions(t *testing.T) {
	// bash and friends preview as the resource text (command), no diff.
	got := previewOf("", "bash", "ls -l", core.ToolCall{
		Input: map[string]any{"command": "ls -l"}})
	if got != "ls -l" {
		t.Errorf("bash preview = %q", got)
	}
	// A file action without a path previews empty.
	if got := previewOf("", "write", "w", core.ToolCall{Input: map[string]any{"content": "x"}}); got != "" {
		t.Errorf("pathless preview = %q", got)
	}
}

// errBackend fails every LLM call (reviewer failure path).
type errBackend struct{}

func (errBackend) Chat(context.Context, core.Request) (core.Response, error) {
	return core.Response{}, errors.New("llm down")
}
func (errBackend) Stream(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
	return core.Response{}, errors.New("llm down")
}

// blockingApprover hangs until the context is cancelled.
type blockingApprover struct{ called chan struct{} }

func (a *blockingApprover) Approve(ctx context.Context, _ core.ApprovalRequest) (bool, error) {
	close(a.called)
	<-ctx.Done()
	return false, ctx.Err()
}

// Cancelling the caller context surfaces as an approval error, not a denial.
func TestGateEngineContextCancelSurfaces(t *testing.T) {
	ap := &blockingApprover{called: make(chan struct{})}
	a, _ := newGatedApp(t, "[review]\nmode = \"human\"\n", func(o *Options) { o.Approver = ap })
	tool, _ := a.Registry().Get("bash")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-ap.called
		cancel()
	}()
	res := tool.Execute(ctx, core.ToolCall{Input: map[string]any{"command": "mkdir c-dir"}})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "approval error") {
		t.Errorf("cancelled review must surface as approval error, got %v", res.Err)
	}
}

func TestLLMReviewer(t *testing.T) {
	rev := &llmReviewer{backend: &stubBackend{responses: []core.Response{textResp("low")}}, model: "judge"}
	v, err := rev.Review(context.Background(), "write", "a.txt", "content=x")
	if err != nil || v != core.RiskLow {
		t.Errorf("Review = %v, %v", v, err)
	}
	// Unparseable reply stays uncertain with no error.
	rev = &llmReviewer{backend: &stubBackend{responses: []core.Response{textResp("hmm maybe")}}, model: "judge"}
	if v, err := rev.Review(context.Background(), "write", "a.txt", "x"); err != nil || v != core.RiskUncertain {
		t.Errorf("Review = %v, %v", v, err)
	}
	// Backend failure surfaces the error.
	rev = &llmReviewer{backend: errBackend{}, model: "judge"}
	if v, err := rev.Review(context.Background(), "write", "a.txt", "x"); err == nil || v != core.RiskUncertain {
		t.Errorf("Review = %v, %v", v, err)
	}
}

func TestParseRiskVerdict(t *testing.T) {
	cases := map[string]core.RiskVerdict{
		"low":       core.RiskLow,
		"High!":     core.RiskHigh,
		"uncertain": core.RiskUncertain,
		"maybe":     core.RiskUncertain,
		"":          core.RiskUncertain,
		"low risk":  core.RiskLow, // first word wins
	}
	for in, want := range cases {
		if got := parseRiskVerdict(in); got != want {
			t.Errorf("parseRiskVerdict(%q) = %v, want %v", in, got, want)
		}
	}
}
