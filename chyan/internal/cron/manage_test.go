package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSchedulerUpsertKeepsRuntimeState: editing a definition must not erase
// last run/status/session; a changed schedule re-arms the next fire.
func TestSchedulerUpsertKeepsRuntimeState(t *testing.T) {
	s := New(Options{Run: func(context.Context, Job) (string, error) { return "", nil }})
	last := time.Now().UTC()
	s.Add(Job{ID: "a", Name: "A", Schedule: "@every 1h", Enabled: true})
	s.mu.Lock()
	j := s.jobs["a"]
	j.LastRunAt = &last
	j.LastStatus = "ok"
	j.LastSessionID = "sess1"
	s.mu.Unlock()

	s.Upsert(Job{ID: "a", Name: "A2", Schedule: "@every 2h", Enabled: false})
	got := s.Snapshot()[0]
	if got.Name != "A2" || got.Enabled {
		t.Errorf("definition not updated: %+v", got)
	}
	if got.LastRunAt == nil || got.LastStatus != "ok" || got.LastSessionID != "sess1" {
		t.Errorf("runtime state lost: %+v", got)
	}
}

// TestSchedulerRunNow: immediate runs report started, refuse disabled/unknown
// jobs and never stack, and record the session plus OnRun notification.
func TestSchedulerRunNow(t *testing.T) {
	var s *Scheduler
	started := make(chan Job, 1)
	s = New(Options{
		Run: func(_ context.Context, j Job) (string, error) {
			s.SetLastSession(j.ID, "sess-run")
			return "ok", nil
		},
		OnRun: func(j Job) { started <- j },
	})
	s.Add(Job{ID: "a", Name: "A", Schedule: "@every 1h", Prompt: "p", Enabled: true})

	ok, err := s.RunNow(context.Background(), "missing")
	if ok || !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("unknown run = %v, %v", ok, err)
	}

	s.Upsert(Job{ID: "a", Name: "A", Schedule: "@every 1h", Prompt: "p", Enabled: false})
	if ok, err := s.RunNow(context.Background(), "a"); ok || !errors.Is(err, ErrJobDisabled) {
		t.Fatalf("disabled run = %v, %v", ok, err)
	}

	s.Upsert(Job{ID: "a", Name: "A", Schedule: "@every 1h", Prompt: "p", Enabled: true})
	ok, err = s.RunNow(context.Background(), "a")
	if !ok || err != nil {
		t.Fatalf("run now = %v, %v", ok, err)
	}
	select {
	case j := <-started:
		if j.LastStatus != "ok" || j.LastSessionID != "sess-run" {
			t.Errorf("OnRun job = %+v", j)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnRun was not called")
	}
}

// TestSchedulerRemoveAndSave: removing a job clears it from the schedule and
// Save serializes the snapshot to jobs.json.
func TestSchedulerRemoveAndSave(t *testing.T) {
	s := New(Options{Run: func(context.Context, Job) (string, error) { return "", nil }})
	s.Add(
		Job{ID: "a", Name: "A", Schedule: "@every 1h", Enabled: true},
		Job{ID: "b", Name: "B", Schedule: "@every 1h", Enabled: true},
	)
	s.Remove("a")
	if got := len(s.Snapshot()); got != 1 {
		t.Fatalf("snapshot has %d jobs, want 1", got)
	}
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"B"`) || strings.Contains(string(data), `"A"`) {
		t.Errorf("saved jobs = %s", data)
	}
}

// TestSchedulerRunPersistsPath: with Options.Path set, each run writes the
// updated runtime state back to the job file.
func TestSchedulerRunPersistsPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	s := New(Options{Path: path, Run: func(context.Context, Job) (string, error) { return "", nil }})
	s.Add(Job{ID: "a", Name: "A", Schedule: "@every 1h", Enabled: true})
	if ok, err := s.RunNow(context.Background(), "a"); !ok || err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), `"last_status": "ok"`) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run state was not persisted to jobs.json")
}

func TestValidateSchedule(t *testing.T) {
	if err := ValidateSchedule("*/5 * * * *"); err != nil {
		t.Errorf("valid schedule rejected: %v", err)
	}
	if err := ValidateSchedule("nope"); err == nil {
		t.Error("invalid schedule accepted")
	}
}

// TestRunNowWhileRunning: a second immediate run while one is in flight is
// refused instead of stacking.
func TestRunNowWhileRunning(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 1)
	s := New(Options{Run: func(context.Context, Job) (string, error) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
		return "", nil
	}})
	s.Add(Job{ID: "a", Name: "A", Schedule: "@every 1h", Enabled: true})
	if ok, err := s.RunNow(context.Background(), "a"); !ok || err != nil {
		t.Fatal(err)
	}
	<-arrived
	if ok, err := s.RunNow(context.Background(), "a"); ok || err != nil {
		t.Fatalf("second run = %v, %v (want refused)", ok, err)
	}
	close(release)
}
