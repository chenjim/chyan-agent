// Hermes jobs.json import (~/.hermes/cron/jobs.json): a {"jobs":[...]} envelope
// whose schedules are {kind,expr} objects. Maps cron/interval/one-shot onto
// chyan job schedules ("@every"/"@at" for the non-cron kinds).
package cron

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// jobIDPattern finds job-id references inside gate scripts.
var jobIDPattern = regexp.MustCompile(`[0-9a-f]{12}`)

// hermesFile is the Hermes jobs.json envelope.
type hermesFile struct {
	Jobs []hermesJob `json:"jobs"`
}

// hermesJob is one Hermes job definition (only the fields we migrate).
type hermesJob struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Prompt      string   `json:"prompt"`
	Deliver     string   `json:"deliver"`
	Enabled     bool     `json:"enabled"`
	WorkDir     string   `json:"workdir"`
	Script      string   `json:"script"`
	NoAgent     bool     `json:"no_agent"`
	ContextFrom []string `json:"context_from"`
	State       string   `json:"state"`
	LastRunAt   string   `json:"last_run_at"`
	LastStatus  string   `json:"last_status"`
	LastError   *string  `json:"last_error"`
	Schedule    struct {
		Kind    string `json:"kind"`
		Expr    string `json:"expr"`
		Minutes int    `json:"minutes"`
		RunAt   string `json:"run_at"`
	} `json:"schedule"`
	Repeat struct {
		Completed int `json:"completed"`
	} `json:"repeat"`
}

// LoadHermes reads a Hermes jobs.json and converts its jobs to chyan jobs.
// Unknown schedule kinds, completed one-shots and unparsable schedules are
// skipped: one bad job must not break the migration.
//
// Gate-script jobs (hermes `script` + short interval polling a wake gate)
// map onto chyan event chains when the gate script names exactly one other
// job in the file: `After: [upstream]`, no schedule (fire on upstream
// completion instead of polling). Gate scripts without a recognizable
// upstream keep their script as a pre-run gate.
func LoadHermes(path string) ([]Job, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cron: load hermes %s: %w", path, err)
	}
	var hf hermesFile
	if err := json.Unmarshal(data, &hf); err != nil {
		return nil, fmt.Errorf("cron: parse hermes %s: %w", path, err)
	}
	ids := map[string]bool{}
	for _, h := range hf.Jobs {
		ids[h.ID] = true
	}
	out := make([]Job, 0, len(hf.Jobs))
	for _, h := range hf.Jobs {
		j := Job{
			ID: h.ID, Name: h.Name,
			Prompt: h.Prompt, Channel: h.Deliver, Enabled: h.Enabled, WorkDir: h.WorkDir,
			Script: h.Script, NoAgent: h.NoAgent, ContextFrom: h.ContextFrom,
		}
		if h.LastError != nil {
			j.LastError = *h.LastError
		}
		j.LastStatus = h.LastStatus
		if t, err := time.Parse(time.RFC3339, h.LastRunAt); err == nil {
			j.LastRunAt = &t
		}
		if up := chainGateUpstream(h, ids); up != "" {
			// Event chain: upstream completion triggers this job once,
			// replacing the 3-minute gate polling entirely.
			j.After = []string{up}
			j.Script = ""
			out = append(out, j)
			continue
		}
		sched, ok := hermesSchedule(h)
		if !ok {
			continue
		}
		j.Schedule = sched
		s, err := parseSchedule(j.Schedule)
		if err != nil {
			continue
		}
		j.sched = s
		out = append(out, j)
	}
	return out, nil
}

// chainGateUpstream reports the upstream job id when a gate-script job is a
// job-completion chain (the script polls exactly one other job id from this
// file, like ai-daily-trigger-gate.py). ok=false for plain pre-run scripts.
func chainGateUpstream(h hermesJob, ids map[string]bool) string {
	if h.Script == "" {
		return ""
	}
	data, err := os.ReadFile(hermesScriptPath(h.Script))
	if err != nil {
		return ""
	}
	found := ""
	for _, id := range jobIDPattern.FindAllString(string(data), -1) {
		if id == h.ID || !ids[id] {
			continue // self-reference or not a job in this file
		}
		if found != "" && found != id {
			return "" // ambiguous: more than one upstream candidate
		}
		found = id
	}
	return found
}

// hermesScriptPath resolves a Hermes script name against the profile's
// scripts dir (hermes's own resolution contract).
func hermesScriptPath(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return name
	}
	return filepath.Join(home, ".hermes", "scripts", name)
}

// hermesSchedule maps one Hermes {kind,...} schedule onto a chyan schedule
// string. ok=false: unknown kind or a one-shot that already ran (never
// migrate stale one-shots; they would fire again).
func hermesSchedule(h hermesJob) (string, bool) {
	switch h.Schedule.Kind {
	case "cron":
		if h.Schedule.Expr == "" {
			return "", false
		}
		return h.Schedule.Expr, true
	case "interval":
		if h.Schedule.Minutes <= 0 {
			return "", false
		}
		return fmt.Sprintf("@every %dm", h.Schedule.Minutes), true
	case "once":
		if h.Repeat.Completed > 0 || h.State == "completed" {
			return "", false // already ran
		}
		t, err := time.Parse(time.RFC3339, h.Schedule.RunAt)
		if err != nil {
			return "", false
		}
		return "@at " + t.Format(time.RFC3339), true
	default:
		return "", false
	}
}
