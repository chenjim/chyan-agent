// Job definition plus jobs.json load/save (Hermes-style JSON array).
package cron

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ErrJobNotFound and ErrJobDisabled report why an immediate run cannot start.
var (
	ErrJobNotFound = fmt.Errorf("cron: job not found")
	ErrJobDisabled = fmt.Errorf("cron: job is disabled")
)

// Job is one cron task definition. Schedule is either a 5-field cron
// expression or "@every <duration>". Channel is an opaque delivery target
// like "feishu:xxx"; this package only stores it, delivery is done by the
// channels layer via the run callback.
//
// Two execution shapes beyond a plain scheduled prompt (both migrated from
// hermes, see LoadHermes):
//   - Script/NoAgent: a gate or pre-run script. NoAgent makes the script the
//     whole job (stdout delivered, no LLM). With a prompt, the script runs
//     first and a trailing {"wakeAgent": false} line skips the agent turn at
//     zero LLM cost ("wake gate", hermes cron/scheduler_prompt semantics).
//   - After: event chaining — the job fires as soon as any listed upstream
//     job finishes, instead of polling for it on a short interval.
type Job struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Schedule      string     `json:"schedule"`
	Prompt        string     `json:"prompt"`
	Channel       string     `json:"channel,omitempty"`
	Enabled       bool       `json:"enabled"`
	WorkDir       string     `json:"workdir,omitempty"`
	Script        string     `json:"script,omitempty"`
	NoAgent       bool       `json:"no_agent,omitempty"`
	After         []string   `json:"after,omitempty"`
	ContextFrom   []string   `json:"context_from,omitempty"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	LastStatus    string     `json:"last_status,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	LastSessionID string     `json:"last_session_id,omitempty"`

	sched schedule // parsed Schedule, filled by Load or Scheduler
}

// Load reads the job list from path. A missing file yields an empty list.
// Jobs with an unparsable Schedule are skipped: one bad job must not break
// the whole schedule.
func Load(path string) ([]Job, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []Job{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cron: load %s: %w", path, err)
	}
	var jobs []Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, fmt.Errorf("cron: parse %s: %w", path, err)
	}
	ok := jobs[:0]
	for _, j := range jobs {
		if j.Schedule == "" && len(j.After) > 0 {
			// Event-only job: no clock schedule, fires from After chaining.
			ok = append(ok, j)
			continue
		}
		s, err := parseSchedule(j.Schedule)
		if err != nil {
			continue // skip bad job, keep the rest schedulable
		}
		j.sched = s
		ok = append(ok, j)
	}
	return ok, nil
}

// Save writes jobs to path as a JSON array. The jobs are the caller's
// snapshot (e.g. from Scheduler.Snapshot); runtime state fields are kept.
func Save(path string, jobs []Job) error {
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return fmt.Errorf("cron: save %s: %w", path, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("cron: save %s: %w", path, err)
	}
	return nil
}
