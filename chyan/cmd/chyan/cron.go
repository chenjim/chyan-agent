// Scheduled job execution for `chyan serve`: loads jobs.json and runs due
// jobs as agent turns, delivering output to the job's channel. Jobs with a
// Script run it first (wake gate: a trailing {"wakeAgent": false} line skips
// the agent turn at zero LLM cost; no_agent jobs are script-only). Hermes
// migration is explicit (`chyan cron import`), never automatic.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/cron"
)

// jobRunner is the app surface one scheduled job needs (test seam).
type jobRunner interface {
	Run(ctx context.Context, sessionID, userInput string) (core.Message, string, error)
	Deliver(ctx context.Context, target, text string) error
}

// scriptTimeout bounds one gate/pre-run script invocation.
const scriptTimeout = 10 * time.Minute

// runJob executes one scheduled job and returns the deliverable output (kept
// by the scheduler for context_from injection) plus the session id opened by
// the agent turn ("" for script-only or gate-closed runs), so the daemon can
// link a job to its latest transcript:
//  1. Script runs first when present; its stdout is the gate and is injected
//     into the prompt as a "## Script Output" block (hermes semantics).
//  2. A trailing {"wakeAgent": false} line skips the agent turn entirely.
//  3. NoAgent jobs are script-only: stdout is the deliverable.
//  4. Otherwise the prompt runs as one agent turn; the reply is delivered to
//     the job's channel ("feishu:oc_x"); jobs without a channel just run.
func runJob(ctx context.Context, r jobRunner, j cron.Job) (string, string, error) {
	prompt := j.Prompt
	if j.Script != "" {
		out, err := runScript(ctx, j)
		if err != nil {
			return "", "", err
		}
		if !wakeGate(out) {
			log.Printf("cron: job %s gate closed (wakeAgent=false), skipping", j.ID)
			return "", "", nil
		}
		if j.NoAgent {
			text := strings.TrimSpace(stripGateLine(out))
			if text == "" {
				return "", "", nil
			}
			if j.Channel == "" {
				return text, "", nil
			}
			return text, "", r.Deliver(ctx, j.Channel, text)
		}
		if s := strings.TrimSpace(stripGateLine(out)); s != "" {
			prompt = "## Script Output\nThe following data was collected by a pre-run script. Use it as context.\n\n```\n" + s + "\n```\n\n" + prompt
		}
	}
	msg, sid, err := r.Run(ctx, "", prompt)
	if err != nil {
		return "", sid, err
	}
	text := core.FlattenText(msg)
	if j.Channel == "" {
		return text, sid, nil
	}
	return text, sid, r.Deliver(ctx, j.Channel, text)
}

// runScript executes the job's script (python/sh by extension) in its work
// dir and returns stdout.
func runScript(ctx context.Context, j cron.Job) (string, error) {
	path := j.Script
	if !filepath.IsAbs(path) {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".hermes", "scripts", path)
		}
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("cron: job %s script: %w", j.ID, err)
	}
	sctx, cancel := context.WithTimeout(ctx, scriptTimeout)
	defer cancel()
	var cmd *exec.Cmd
	switch filepath.Ext(path) {
	case ".py":
		cmd = exec.CommandContext(sctx, "python3", path)
	case ".sh":
		cmd = exec.CommandContext(sctx, "bash", path)
	default:
		cmd = exec.CommandContext(sctx, path)
	}
	cmd.Dir = j.WorkDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("cron: job %s script: %w", j.ID, err)
	}
	return out.String(), nil
}

// wakeGate reports whether the agent turn should run: false only when the
// last non-empty stdout line is JSON {"wakeAgent": false} (hermes
// _parse_wake_gate; anything else wakes normally).
func wakeGate(scriptOutput string) bool {
	lines := strings.Split(scriptOutput, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var gate map[string]any
		if err := json.Unmarshal([]byte(line), &gate); err != nil {
			return true
		}
		v, ok := gate["wakeAgent"].(bool)
		return !ok || v
	}
	return true
}

// stripGateLine drops the trailing wake-gate JSON line from script output so
// it never reaches the prompt.
func stripGateLine(scriptOutput string) string {
	lines := strings.Split(scriptOutput, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var gate map[string]any
		if err := json.Unmarshal([]byte(line), &gate); err == nil {
			if _, ok := gate["wakeAgent"]; ok {
				lines = append(lines[:i], lines[i+1:]...)
			}
		}
		break
	}
	return strings.Join(lines, "\n")
}

// startScheduler loads the job list and starts the cron loop in the
// background. Migration from hermes is explicit (`chyan cron import`),
// never automatic: nothing is scheduled until the user imports or writes
// jobs.json. onRun, when non-nil, receives each updated job after a run
// (the daemon publishes it over SSE; tests pass nil).
func startScheduler(ctx context.Context, r jobRunner, home string, onRun func(cron.Job)) *cron.Scheduler {
	jobsPath := filepath.Join(home, "jobs.json")
	jobs, err := cron.Load(jobsPath)
	if err != nil {
		log.Printf("cron: load jobs failed: %v", err)
	}
	// Per-job durable state: every fire is a fresh session, so phase/attempts/
	// last output must be restored from disk (AutoResearch "stateless trigger,
	// stateful file"). A crash between BeginRun and FinishRun leaves phase
	// "running" with an incremented attempt count on the next load.
	stateDir := filepath.Join(home, "state", "cron")
	var sched *cron.Scheduler
	sched = cron.New(cron.Options{Path: jobsPath, OnRun: onRun, Run: func(ctx context.Context, j cron.Job) (string, error) {
		if j.ID == "" { // nothing to key the state file on
			out, _, err := runJob(ctx, r, j)
			return out, err
		}
		st, err := cron.LoadJobState(stateDir, j.ID)
		if err != nil {
			log.Printf("cron: load state for %s failed: %v", j.ID, err)
		}
		j.Prompt = cron.StatePrompt(st, j.Prompt)
		st = cron.BeginRun(st)
		if err := cron.SaveJobState(stateDir, st); err != nil {
			log.Printf("cron: save state for %s failed: %v", j.ID, err)
		}
		out, sid, runErr := runJob(ctx, r, j)
		if sid != "" {
			// Recorded before returning so the scheduler's post-run snapshot
			// and OnRun callback (daemon SSE) already carry the session id.
			sched.SetLastSession(j.ID, sid)
		}
		st = cron.FinishRun(st, out, runErr)
		if err := cron.SaveJobState(stateDir, st); err != nil {
			log.Printf("cron: save state for %s failed: %v", j.ID, err)
		}
		return out, runErr
	}})
	sched.Add(jobs...)
	sched.Start(ctx)
	log.Printf("cron: %d job(s) scheduled", len(sched.Snapshot()))
	return sched
}

// importHermesJobs imports ~/.hermes/cron/jobs.json into jobsPath
// (`chyan cron import`). Returns the migrated jobs, or nil when there is
// nothing to migrate or the save failed (never report success on failure).
func importHermesJobs(jobsPath string) []cron.Job {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	hermesPath := filepath.Join(home, ".hermes", "cron", "jobs.json")
	if _, err := os.Stat(hermesPath); err != nil {
		return nil
	}
	jobs, err := cron.LoadHermes(hermesPath)
	if err != nil {
		log.Printf("cron: hermes migration failed: %v", err)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(jobsPath), 0o755); err != nil {
		log.Printf("cron: create %s failed: %v", filepath.Dir(jobsPath), err)
		return nil
	}
	if err := cron.Save(jobsPath, jobs); err != nil {
		log.Printf("cron: save migrated jobs failed: %v", err)
		return nil
	}
	log.Printf("cron: migrated %d job(s) from hermes", len(jobs))
	return jobs
}

// cronCmd exposes explicit job management (`chyan cron import`). Import is
// deliberately manual: hermes tasks must never start executing without the
// user asking for it.
func cronCmd() *cobra.Command {
	f := &runFlags{}
	cmd := &cobra.Command{Use: "cron", Short: "Manage scheduled jobs"}
	importC := &cobra.Command{
		Use:   "import",
		Short: "Import cron jobs from ~/.hermes/cron/jobs.json into <home>/jobs.json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			home := f.home
			if home == "" {
				var err error
				if home, err = defaultHome(); err != nil {
					return err
				}
			}
			jobsPath := filepath.Join(home, "jobs.json")
			jobs := importHermesJobs(jobsPath)
			if len(jobs) == 0 {
				fmt.Fprintln(os.Stdout, "没有导入任何任务（hermes jobs.json 不存在/为空，或保存失败，见上方日志）。")
				return nil
			}
			fmt.Fprintf(os.Stdout, "已导入 %d 个任务到 %s：\n", len(jobs), jobsPath)
			for _, j := range jobs {
				sched := j.Schedule
				if sched == "" {
					sched = "after " + strings.Join(j.After, ",")
				}
				fmt.Fprintf(os.Stdout, "  %-32s %-20s enabled=%v\n", j.Name, sched, j.Enabled)
			}
			fmt.Fprintln(os.Stdout, "注意：任务默认按迁移状态调度；确认前可在 jobs.json 中改 enabled。")
			return nil
		},
	}
	bindRunFlags(importC, f)
	cmd.AddCommand(importC)
	return cmd
}

// defaultHome resolves the chyan data root like app.New does.
func defaultHome() (string, error) {
	if h := os.Getenv("CHYAN_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".chyan"), nil
}
