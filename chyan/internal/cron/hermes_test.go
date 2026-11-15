package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadHermes converts a realistic Hermes jobs.json: cron/interval/once
// all map, completed one-shots and unknown kinds are skipped.
func TestLoadHermes(t *testing.T) {
	raw := `{
	  "jobs": [
	    {
	      "id": "cron1", "name": "market", "prompt": "run market.py",
	      "deliver": "feishu", "enabled": true, "state": "scheduled",
	      "schedule": {"kind": "cron", "expr": "38 14 * * 1-5", "display": "38 14 * * 1-5"},
	      "repeat": {"times": null, "completed": 121},
	      "last_run_at": "2026-10-15T14:39:00.157387+08:00",
	      "last_status": "ok", "last_error": null
	    },
	    {
	      "id": "iv1", "name": "disk alert", "prompt": "check disk",
	      "deliver": "feishu", "enabled": true, "state": "scheduled",
	      "schedule": {"kind": "interval", "minutes": 120, "display": "every 120m"},
	      "repeat": {"times": null, "completed": 10}
	    },
	    {
	      "id": "once1", "name": "rebalance", "prompt": "rebalance now",
	      "deliver": "feishu", "enabled": true, "state": "scheduled",
	      "schedule": {"kind": "once", "run_at": "2026-10-16T11:00:00+08:00"},
	      "repeat": {"times": null, "completed": 0}
	    },
	    {
	      "id": "done1", "name": "already ran", "prompt": "x",
	      "deliver": "feishu", "enabled": true, "state": "completed",
	      "schedule": {"kind": "once", "run_at": "2026-01-01T00:00:00+08:00"},
	      "repeat": {"times": null, "completed": 1}
	    },
	    {
	      "id": "bad1", "name": "unknown kind", "prompt": "x",
	      "deliver": "feishu", "enabled": true, "state": "scheduled",
	      "schedule": {"kind": "lunar", "expr": "x"}
	    },
	    {
	      "id": "bad2", "name": "unparsable cron", "prompt": "x",
	      "deliver": "feishu", "enabled": true, "state": "scheduled",
	      "schedule": {"kind": "cron", "expr": "not a cron"}
	    }
	  ]
	}`
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	jobs, err := LoadHermes(path)
	if err != nil {
		t.Fatalf("LoadHermes: %v", err)
	}
	byID := map[string]Job{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	// cron1/iv1/once1 migrate; done1 (completed one-shot), bad1 (unknown
	// kind) and bad2 (unparsable) are skipped.
	if len(jobs) != 3 {
		t.Fatalf("migrated %d jobs, want 3: %+v", len(jobs), byID)
	}
	c := byID["cron1"]
	if c.Schedule != "38 14 * * 1-5" || c.Channel != "feishu" || !c.Enabled {
		t.Errorf("cron1 = %+v", c)
	}
	if c.LastStatus != "ok" || c.LastRunAt == nil {
		t.Errorf("cron1 runtime state = %+v", c)
	}
	if got := byID["iv1"].Schedule; got != "@every 120m" {
		t.Errorf("iv1 schedule = %q", got)
	}
	if got := byID["once1"].Schedule; !strings.HasPrefix(got, "@at ") {
		t.Errorf("once1 schedule = %q", got)
	}
	// Every migrated job is schedulable by the scheduler itself.
	s := New(Options{Run: func(context.Context, Job) (string, error) { return "", nil }})
	s.Add(jobs...)
	if len(s.Snapshot()) != 3 {
		t.Errorf("scheduler accepted %d, want 3", len(s.Snapshot()))
	}
}

// TestLoadHermesErrors: missing file and bad JSON surface as errors.
func TestLoadHermesErrors(t *testing.T) {
	if _, err := LoadHermes(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("missing file must error")
	}
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHermes(path); err == nil {
		t.Error("bad JSON must error")
	}
}

// TestAtOnceSchedule: "@at <RFC3339>" fires exactly once at its time.
func TestAtOnceSchedule(t *testing.T) {
	when := time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)
	s, err := parseSchedule("@at " + when.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("parseSchedule: %v", err)
	}
	before := when.Add(-time.Minute)
	if got := s.next(before); !got.Equal(when) {
		t.Errorf("next before = %v, want %v", got, when)
	}
	// After it fires, no further occurrence (one-shot is done).
	if got := s.next(when.Add(time.Minute)); !got.IsZero() {
		t.Errorf("next after fire = %v, want zero", got)
	}
	// Bad @at time is rejected.
	if _, err := parseSchedule("@at not-a-time"); err == nil {
		t.Error("bad @at must error")
	}
}

// TestLoadHermesChainGate: a gate-script job polling exactly one other job
// id in the file migrates to an After event chain (no more 3m polling).
func TestLoadHermesChainGate(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	scriptDir := filepath.Join(home, ".hermes", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gate := "#!/usr/bin/env python3\nUPSTREAM_JOB = '9813d2b72620'\n"
	if err := os.WriteFile(filepath.Join(scriptDir, "chain-gate.py"), []byte(gate), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := `{
	  "jobs": [
	    {
	      "id": "9813d2b72620", "name": "upstream", "prompt": "pick topic",
	      "deliver": "feishu", "enabled": true,
	      "schedule": {"kind": "cron", "expr": "18 7 * * *"}
	    },
	    {
	      "id": "edff31801785", "name": "downstream", "prompt": "publish",
	      "deliver": "feishu", "enabled": true,
	      "script": "chain-gate.py",
	      "schedule": {"kind": "interval", "minutes": 3}
	    }
	  ]
	}`
	path := filepath.Join(dir, "jobs.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs, err := LoadHermes(path)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Job{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	d := byID["edff31801785"]
	if len(d.After) != 1 || d.After[0] != "9813d2b72620" {
		t.Errorf("downstream After = %v, want [9813d2b72620]", d.After)
	}
	if d.Schedule != "" {
		t.Errorf("chained job keeps schedule %q, want empty (event-only)", d.Schedule)
	}
	if d.Script != "" {
		t.Errorf("chained job keeps script %q, want dropped", d.Script)
	}
}
