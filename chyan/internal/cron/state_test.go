package cron

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := JobState{JobID: "job/1", Falsified: []string{"feature-x"}, Evidence: []string{"/tmp/a"}}
	if err := SaveJobState(dir, st); err != nil {
		t.Fatal(err)
	}
	got, err := LoadJobState(dir, "job/1")
	if err != nil {
		t.Fatal(err)
	}
	if got.JobID != "job/1" || len(got.Falsified) != 1 || got.Falsified[0] != "feature-x" {
		t.Errorf("round trip = %+v", got)
	}
	if got.Phase != PhaseIdle {
		t.Errorf("phase = %q", got.Phase)
	}
}

func TestLoadMissingStateIsIdle(t *testing.T) {
	st, err := LoadJobState(t.TempDir(), "nope")
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != PhaseIdle || st.Attempts != 0 {
		t.Errorf("missing state = %+v", st)
	}
}

func TestBeginAndFinishRun(t *testing.T) {
	st := BeginRun(JobState{JobID: "a"})
	if st.Phase != PhaseRunning || st.Attempts != 1 {
		t.Errorf("begin = %+v", st)
	}
	st = FinishRun(st, "partial output", errors.New("llm down"))
	if st.Phase != PhaseFailed || st.LastStatus != "error" || st.LastError != "llm down" {
		t.Errorf("failed finish = %+v", st)
	}
	st = FinishRun(st, "ok output", nil)
	if st.Phase != PhaseIdle || st.LastStatus != "ok" || st.LastError != "" {
		t.Errorf("ok finish = %+v", st)
	}
	if st.LastOutput != "ok output" {
		t.Errorf("last output = %q", st.LastOutput)
	}
}

func TestStatePromptPristineUntouched(t *testing.T) {
	if got := StatePrompt(JobState{Phase: PhaseIdle}, "do work"); got != "do work" {
		t.Errorf("pristine prompt changed: %q", got)
	}
}

func TestStatePromptCarriesResumeFacts(t *testing.T) {
	st := JobState{Phase: PhaseFailed, Attempts: 2, LastError: "timeout",
		Falsified: []string{"retry same model"}, LastOutput: "previous answer"}
	got := StatePrompt(st, "do work")
	for _, want := range []string{"## Job State", "failed", "attempt: 3", "timeout",
		"retry same model", "previous answer", "never reuse a value"} {
		if !strings.Contains(got, want) {
			t.Errorf("state prompt missing %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "do work") {
		t.Error("original prompt must stay at the end")
	}
}

func TestSaveJobStateIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := SaveJobState(dir, JobState{JobID: "x", Phase: PhaseRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.json.tmp")); !os.IsNotExist(err) {
		t.Error("temp file must be renamed away")
	}
	data, err := os.ReadFile(filepath.Join(dir, "x.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), PhaseRunning) {
		t.Errorf("state file = %s", data)
	}
}
