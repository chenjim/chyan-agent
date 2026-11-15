// Durable per-job state: every trigger opens a fresh session, so the JSON
// state file is the only truth that survives between runs (phase, attempts,
// last failure, last output head). Volatile values (task ids, file mtimes)
// must be re-read from the environment each run, never reused from here.
package cron

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Job phases written by the scheduler. The agent may add finer phases later;
// unknown values are passed through unchanged.
const (
	PhaseIdle    = "idle"
	PhaseRunning = "running"
	PhaseFailed  = "failed"
)

// JobState is the persisted resume record for one job.
type JobState struct {
	JobID      string    `json:"job_id"`
	Phase      string    `json:"phase"`
	Attempts   int       `json:"attempts"`
	LastStatus string    `json:"last_status,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	LastOutput string    `json:"last_output,omitempty"` // head of the last answer, for resume
	Falsified  []string  `json:"falsified,omitempty"`   // directions already disproved
	Evidence   []string  `json:"evidence,omitempty"`    // artifact paths / hashes
	UpdatedAt  time.Time `json:"updated_at"`
}

// LoadJobState reads one job's state; a missing file yields an idle state.
func LoadJobState(dir, jobID string) (JobState, error) {
	if jobID == "" {
		return JobState{}, fmt.Errorf("cron: empty job id")
	}
	path := statePath(dir, jobID)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return JobState{JobID: jobID, Phase: PhaseIdle}, nil
	}
	if err != nil {
		return JobState{JobID: jobID}, err
	}
	var st JobState
	if err := json.Unmarshal(data, &st); err != nil {
		return JobState{JobID: jobID}, fmt.Errorf("cron: parse state %s: %w", path, err)
	}
	if st.JobID == "" {
		st.JobID = jobID
	}
	if st.Phase == "" {
		st.Phase = PhaseIdle
	}
	return st, nil
}

// SaveJobState writes the state atomically (temp file + rename) so a crash
// never leaves a half-written resume record.
func SaveJobState(dir string, st JobState) error {
	if st.JobID == "" {
		return fmt.Errorf("cron: state has no job id")
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := statePath(dir, st.JobID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// BeginRun marks an attempt as started before the job body runs, so a crash
// still shows "running" with an incremented counter on the next load.
func BeginRun(st JobState) JobState {
	st.Attempts++
	st.Phase = PhaseRunning
	st.UpdatedAt = time.Now().UTC()
	return st
}

// FinishRun records the outcome. Output is truncated to a head excerpt: the
// full answer belongs to the session store, not the resume record.
func FinishRun(st JobState, output string, runErr error) JobState {
	st.LastStatus = "ok"
	st.LastError = ""
	if runErr != nil {
		st.Phase = PhaseFailed
		st.LastStatus = "error"
		st.LastError = runErr.Error()
	} else {
		st.Phase = PhaseIdle
	}
	if output != "" {
		st.LastOutput = headText(output, 2048)
	}
	st.UpdatedAt = time.Now().UTC()
	return st
}

// StatePrompt prepends the resume block to the job prompt. A pristine state
// (first run, nothing recorded) is left untouched to avoid prompt noise.
func StatePrompt(st JobState, prompt string) string {
	hasRecord := st.Attempts > 0 || st.LastOutput != "" || st.LastError != "" ||
		len(st.Falsified) > 0 || len(st.Evidence) > 0
	if !hasRecord {
		return prompt
	}
	var b strings.Builder
	b.WriteString("## Job State\n")
	b.WriteString("This run starts a fresh session. The state file below is the only resume record.\n")
	b.WriteString("- phase: ")
	b.WriteString(orDefault(st.Phase, PhaseIdle))
	b.WriteString("\n")
	fmt.Fprintf(&b, "- attempt: %d\n", st.Attempts+1)
	if st.LastError != "" {
		b.WriteString("- last error: ")
		b.WriteString(clipState(st.LastError, 500))
		b.WriteString("\n")
	}
	if len(st.Falsified) > 0 {
		b.WriteString("- falsified (do not retry, choose another direction):\n")
		for _, f := range st.Falsified {
			b.WriteString("  - ")
			b.WriteString(clipState(f, 300))
			b.WriteString("\n")
		}
	}
	if len(st.Evidence) > 0 {
		b.WriteString("- evidence (original artifacts):\n")
		for _, e := range st.Evidence {
			b.WriteString("  - ")
			b.WriteString(clipState(e, 300))
			b.WriteString("\n")
		}
	}
	if st.LastOutput != "" {
		b.WriteString("\n### Last output (head)\n")
		b.WriteString(st.LastOutput)
		b.WriteString("\n")
	}
	b.WriteString("\nVolatile values (task ids, run ids, file mtimes) must be read again this run; never reuse a value from this block.\n\n")
	b.WriteString(prompt)
	return b.String()
}

func statePath(dir, jobID string) string {
	return filepath.Join(dir, sanitizeID(jobID)+".json")
}

// sanitizeID keeps state filenames portable for arbitrary job ids.
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 64 {
			break
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return "job"
	}
	return out
}

func headText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…[truncated]"
}

func clipState(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
