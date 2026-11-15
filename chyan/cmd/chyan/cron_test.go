package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/cron"
)

// stubJobRunner records job runs and deliveries.
type stubJobRunner struct {
	ran      []string
	deliver  []string
	reply    string
	runErr   error
	replyErr error
}

func (s *stubJobRunner) Run(_ context.Context, sessionID, input string) (core.Message, string, error) {
	s.ran = append(s.ran, input)
	if s.runErr != nil {
		return core.Message{}, "", s.runErr
	}
	return core.Message{Role: core.RoleAssistant,
		Content: []core.Part{{Type: "text", Text: s.reply}}}, "ses-1", nil
}

func (s *stubJobRunner) Deliver(_ context.Context, target, text string) error {
	if s.replyErr != nil {
		return s.replyErr
	}
	s.deliver = append(s.deliver, target+"|"+text)
	return nil
}

// TestRunJob: prompt runs as one agent turn and output is delivered to the
// job's channel; channel-less jobs just run.
func TestRunJob(t *testing.T) {
	r := &stubJobRunner{reply: "report content"}
	j := cron.Job{ID: "1", Prompt: "do the thing", Channel: "feishu:oc_a"}
	out, sid, err := runJob(context.Background(), r, j)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if out != "report content" {
		t.Errorf("out = %q", out)
	}
	if sid != "ses-1" {
		t.Errorf("session id = %q, want the runner's session", sid)
	}
	if len(r.ran) != 1 || r.ran[0] != "do the thing" {
		t.Errorf("ran = %v", r.ran)
	}
	if len(r.deliver) != 1 || r.deliver[0] != "feishu:oc_a|report content" {
		t.Errorf("deliver = %v", r.deliver)
	}

	// No channel: no delivery.
	r2 := &stubJobRunner{reply: "x"}
	if _, _, err := runJob(context.Background(), r2, cron.Job{Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	if len(r2.deliver) != 0 {
		t.Errorf("deliver without channel = %v", r2.deliver)
	}

	// Run failure and delivery failure surface.
	r3 := &stubJobRunner{runErr: errors.New("llm down")}
	if _, _, err := runJob(context.Background(), r3, cron.Job{Prompt: "p"}); err == nil {
		t.Error("run error must surface")
	}
	r4 := &stubJobRunner{reply: "x", replyErr: errors.New("im down")}
	if _, _, err := runJob(context.Background(), r4, cron.Job{Prompt: "p", Channel: "feishu:x"}); err == nil {
		t.Error("delivery error must surface")
	}
}

// TestRunJobWakeGate: a script ending in {"wakeAgent": false} skips the
// agent turn entirely (zero LLM); otherwise its output is injected.
func TestRunJobWakeGate(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate.sh")
	if err := os.WriteFile(gate, []byte("echo '{\"wakeAgent\": false}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &stubJobRunner{reply: "x"}
	out, _, err := runJob(context.Background(), r, cron.Job{ID: "g", Prompt: "p", Script: gate})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" || len(r.ran) != 0 {
		t.Errorf("gate closed must skip agent: out=%q ran=%v", out, r.ran)
	}

	// Gate open: the script output is injected into the prompt.
	if err := os.WriteFile(gate, []byte("echo picked-topic\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r2 := &stubJobRunner{reply: "x"}
	if _, _, err := runJob(context.Background(), r2, cron.Job{ID: "g", Prompt: "p", Script: gate}); err != nil {
		t.Fatal(err)
	}
	if len(r2.ran) != 1 || !strings.Contains(r2.ran[0], "picked-topic") {
		t.Errorf("script output not injected: %v", r2.ran)
	}

	// no_agent: the script IS the job, stdout delivered verbatim.
	r3 := &stubJobRunner{reply: "unused"}
	out3, _, err := runJob(context.Background(), r3, cron.Job{ID: "g", Script: gate, NoAgent: true, Channel: "feishu:c"})
	if err != nil {
		t.Fatal(err)
	}
	if out3 != "picked-topic" || len(r3.ran) != 0 {
		t.Errorf("no_agent out=%q ran=%v", out3, r3.ran)
	}
}

// TestStartSchedulerSchedulesJobs: jobs.json is loaded and started.
func TestStartSchedulerSchedulesJobs(t *testing.T) {
	home := t.TempDir()
	jobs := []cron.Job{{ID: "a", Schedule: "@every 1h", Prompt: "p", Enabled: true}}
	if err := cron.Save(filepath.Join(home, "jobs.json"), jobs); err != nil {
		t.Fatal(err)
	}
	r := &stubJobRunner{reply: "x"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := startScheduler(ctx, r, home, nil)
	defer s.Stop()
	if got := len(s.Snapshot()); got != 1 {
		t.Errorf("scheduled %d jobs, want 1", got)
	}
}

// TestImportHermesJobs: explicit import writes jobs.json once; without a
// hermes file there is nothing to import.
func TestImportHermesJobs(t *testing.T) {
	home := t.TempDir()
	hermesDir := filepath.Join(home, ".hermes", "cron")
	if err := os.MkdirAll(hermesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"jobs":[{"id":"j1","name":"n","prompt":"p","deliver":"feishu",
		"enabled":true,"schedule":{"kind":"cron","expr":"0 9 * * *"}}]}`
	if err := os.WriteFile(filepath.Join(hermesDir, "jobs.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home) // os.UserHomeDir respects $HOME

	jobsPath := filepath.Join(home, "jobs.json")
	got := importHermesJobs(jobsPath)
	if len(got) != 1 || got[0].Schedule != "0 9 * * *" {
		t.Fatalf("migrated = %+v", got)
	}
	// The migrated file exists now.
	if _, err := os.Stat(jobsPath); err != nil {
		t.Fatalf("migrated jobs not saved: %v", err)
	}
	// No hermes file: nothing to migrate.
	if err := os.Remove(filepath.Join(hermesDir, "jobs.json")); err != nil {
		t.Fatal(err)
	}
	if got := importHermesJobs(jobsPath); got != nil {
		t.Errorf("import without hermes file = %+v", got)
	}
}

// TestStartSchedulerDoesNotAutoMigrate: an empty jobs.json must NOT pull in
// hermes tasks behind the user's back (import is explicit via `chyan cron
// import`; silent auto-execution of migrated tasks was a real incident).
func TestStartSchedulerDoesNotAutoMigrate(t *testing.T) {
	home := t.TempDir()
	hermesDir := filepath.Join(home, ".hermes", "cron")
	if err := os.MkdirAll(hermesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"jobs":[{"id":"j1","name":"n","prompt":"p","enabled":true,
		"schedule":{"kind":"interval","minutes":5}}]}`
	if err := os.WriteFile(filepath.Join(hermesDir, "jobs.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := startScheduler(ctx, &stubJobRunner{reply: "x"}, home, nil)
	defer s.Stop()
	if got := len(s.Snapshot()); got != 0 {
		t.Errorf("auto-migrated %d jobs, want 0 (import must be explicit)", got)
	}
	if _, err := os.Stat(filepath.Join(home, "jobs.json")); !os.IsNotExist(err) {
		t.Errorf("jobs.json must not materialize without explicit import, err=%v", err)
	}
}
