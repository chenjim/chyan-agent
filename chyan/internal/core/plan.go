// Plan-and-Execute orchestration: a Planner phase that researches read-only
// and finalizes a write plan, and an Executor phase that carries the plan out.
// The two phases are two run modes of one agent (toggled in the REPL), not two
// separate agents. Both share the main model and use built-in, non-overridable
// prompts (product decision: no per-phase prompt or model configuration).
package core

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Phase selects the run mode. Executor is the default (ReAct free-run when no
// plan exists); Planner is the read-only planning phase.
type Phase string

const (
	// PhaseExecutor runs the tool loop; with no plan it is plain ReAct, with a
	// plan it executes the plan on an execution-intent turn.
	PhaseExecutor Phase = "executor"
	// PhasePlanner researches read-only and finalizes a plan via submit_plan.
	// It never performs writes (tools are filtered, see PlannerRegistry).
	PhasePlanner Phase = "planner"
)

// PlanStep is one step of a finalized write plan.
type PlanStep struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	// Status tracks step progress: pending | done | failed | skipped. The
	// Executor updates it as steps complete; pending is the initial value.
	Status string `json:"status"`
}

// Step status values.
const (
	StepPending = "pending"
	StepDone    = "done"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// Plan is the finalized write plan produced by the Planner phase and carried
// into the Executor phase.
type Plan struct {
	Goal        string     `json:"goal"`
	Steps       []PlanStep `json:"steps"`
	GeneratedAt time.Time  `json:"generated_at"`
}

// Empty reports whether the plan carries no actionable steps.
func (p Plan) Empty() bool { return len(p.Steps) == 0 }

// PlanStore persists a session's plan (session KV attachment). internal/app
// adapts its store to this; core keeps the dependency narrow.
type PlanStore interface {
	SavePlan(ctx context.Context, sessionID string, p Plan) error
	LoadPlan(ctx context.Context, sessionID string) (Plan, error)
}

// PlannerSystemPrompt instructs the read-only planning phase. It emphasizes
// the hard no-write constraint (belt and braces: the tool layer already drops
// write tools) and the finalize-via-submit_plan flow.
const PlannerSystemPrompt = `You are the Planner of a plan-and-Execute agent.

Your job is to RESEARCH READ-ONLY and produce a write plan. You NEVER perform
writes yourself: the write tools are not available and any write command is
rejected. Use only read/inspect tools (read, list, search, grep, web_*, bash
read-only commands) to understand the current state.

Process:
1. Inspect the relevant files and context to ground the plan in reality.
2. Propose a concrete write plan: which files to create/modify and how.
   When the user faces a real choice, lay out 2-3 options with trade-offs and
   let the user pick before finalizing.
3. When the plan is settled, call submit_plan with a goal and ordered steps.
   Each step names the target file and the change. Do NOT execute anything.

Be concise. The plan must be specific enough that an executor can carry it out
without re-deciding what to do.`

// ExecutorSystemPrompt instructs the plan-execution phase. The finalized plan
// is appended after it (see RenderPlan). Intent gate: only an execution-intent
// user turn starts work; clarification turns stay conversational. On a clearly
// wrong plan, execute what is safe and report the deviation instead of silently
// re-planning.
const ExecutorSystemPrompt = `You are the Executor of a plan-and-Execute agent.
A finalized write plan is provided below. Follow it.

Rules:
- Intent gate: only when the user's message expresses execution intent (e.g.
  "执行", "开始", "动手", "run it") do you start carrying out the plan. If the
  user is asking a question, clarifying, or adjusting the plan, respond in
  conversation and do NOT start writing.
- When executing, follow the plan step by step. Report progress as you go.
- If a plan step is clearly wrong once you look at the real files, do the safe
  part you can and, when finished, clearly tell the user what deviated and why
  instead of silently inventing a different plan.`

// RenderPlan formats a plan as an LLM-facing block appended to the executor
// prompt.
func RenderPlan(p Plan) string {
	if p.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Finalized plan\nGoal: ")
	b.WriteString(p.Goal)
	b.WriteString("\n")
	for i, s := range p.Steps {
		status := s.Status
		if status == "" {
			status = StepPending
		}
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, status, s.Title)
		if s.Detail != "" {
			b.WriteString(" — ")
			b.WriteString(s.Detail)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ------------------------------------------------------------- submit_plan

// SubmitPlanTool finalizes the Planner phase's write plan (the only mutation
// the Planner performs: it records the plan, never edits user files). The tool
// is bound to one session so the plan lands on the right session attachment.
type SubmitPlanTool struct {
	store   PlanStore
	session string
}

// NewSubmitPlanTool wires the plan store and target session (nil store
// disables the tool with a clear error).
func NewSubmitPlanTool(store PlanStore, session string) *SubmitPlanTool {
	return &SubmitPlanTool{store: store, session: session}
}

// Name implements Tool.
func (t *SubmitPlanTool) Name() string { return "submit_plan" }

// Schema implements Tool.
func (t *SubmitPlanTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"goal": map[string]any{"type": "string", "description": "one-line objective of the plan"},
			"steps": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title":  map[string]any{"type": "string", "description": "short step title"},
						"detail": map[string]any{"type": "string", "description": "target file and the change to make"},
					},
					"required": []string{"title"},
				},
			},
		},
		"required": []string{"goal", "steps"},
	}
}

// Execute implements Tool: it records the finalized plan for the session.
func (t *SubmitPlanTool) Execute(ctx context.Context, call ToolCall) ToolResult {
	if t.store == nil {
		return ToolResult{Err: fmt.Errorf("submit_plan: plan store not configured")}
	}
	goal, _ := call.Input["goal"].(string)
	if strings.TrimSpace(goal) == "" {
		return ToolResult{Err: fmt.Errorf("submit_plan: goal is required")}
	}
	rawSteps, _ := call.Input["steps"].([]any)
	if len(rawSteps) == 0 {
		return ToolResult{Err: fmt.Errorf("submit_plan: at least one step is required")}
	}
	plan := Plan{Goal: strings.TrimSpace(goal), GeneratedAt: nowUTC()}
	for _, item := range rawSteps {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		title, _ := m["title"].(string)
		if strings.TrimSpace(title) == "" {
			continue
		}
		detail, _ := m["detail"].(string)
		plan.Steps = append(plan.Steps, PlanStep{
			Title: strings.TrimSpace(title), Detail: strings.TrimSpace(detail),
			Status: StepPending,
		})
	}
	if len(plan.Steps) == 0 {
		return ToolResult{Err: fmt.Errorf("submit_plan: no valid steps")}
	}
	if err := t.store.SavePlan(ctx, t.session, plan); err != nil {
		return ToolResult{Err: fmt.Errorf("submit_plan: %w", err)}
	}
	return ToolResult{Content: fmt.Sprintf("plan saved: %d step(s)", len(plan.Steps))}
}

// ------------------------------------------------------ planner tool filter

// plannerAllow lists the tools the Planner phase may use: read-only research
// plus plan submission. Everything else — file writes, memory writes,
// sub-agents (whose children could write), media generation, and MCP tools of
// unknown side effects — is dropped (default-deny).
var plannerAllow = map[string]bool{
	"read": true, "list": true, "search": true, "grep": true,
	"web_fetch": true, "web_search": true, "browser": true,
	"read_session": true, "recall": true,
	"bash": true, "submit_plan": true,
}

// PlannerRegistry builds the Planner phase's read-only tool view from src:
// allowed tools pass through, "bash" is wrapped in the read-only command
// guard, and the submit_plan tool is added. Tools not on the allow list are
// dropped. Returns a fresh registry; src is unchanged.
func PlannerRegistry(src *ToolRegistry, submit Tool) *ToolRegistry {
	out := NewToolRegistry()
	for _, t := range src.Tools() {
		if !plannerAllow[t.Name()] {
			continue
		}
		if t.Name() == "bash" {
			t = NewReadOnlyCommandGuard(t)
		}
		_ = out.Register(t)
	}
	if submit != nil {
		_ = out.Register(submit)
	}
	return out
}

// ------------------------------------------------- read-only bash guard

// ReadOnlyCommandGuard wraps a command tool (bash) and rejects any command
// that could write. Used in the Planner phase so "read anything, write
// nothing" holds even through bash.
type ReadOnlyCommandGuard struct{ inner Tool }

// NewReadOnlyCommandGuard wraps a command tool in the read-only guard.
func NewReadOnlyCommandGuard(inner Tool) *ReadOnlyCommandGuard {
	return &ReadOnlyCommandGuard{inner: inner}
}

// Name implements Tool.
func (g *ReadOnlyCommandGuard) Name() string { return g.inner.Name() }

// Schema implements Tool.
func (g *ReadOnlyCommandGuard) Schema() map[string]any { return g.inner.Schema() }

// Execute implements Tool: it forwards only read-only commands.
func (g *ReadOnlyCommandGuard) Execute(ctx context.Context, call ToolCall) ToolResult {
	cmd, _ := call.Input["command"].(string)
	if reason := writeReason(cmd); reason != "" {
		return ToolResult{Err: fmt.Errorf("planner is read-only: rejected %q (%s)", cmd, reason)}
	}
	return g.inner.Execute(ctx, call)
}

// shellSegment splits a command into segments on shell separators (; && || |).
var shellSegment = regexp.MustCompile(`[|;&]+`)

// writeReason reports why a shell command is considered a write ("" = safe).
// Deliberately conservative: an over-rejection just asks the planner to pick a
// different read command, while an under-rejection would mutate user files.
func writeReason(cmd string) string {
	lower := strings.ToLower(cmd)
	// Output redirection / append / tee write files. fd redirects like 2>&1
	// are rare in research and lumped in: conservative is fine here.
	if strings.Contains(lower, ">") || strings.Contains(lower, "tee") {
		return "output redirection"
	}
	for _, tok := range shellSegment.Split(lower, -1) {
		fields := strings.Fields(strings.TrimSpace(tok))
		if len(fields) == 0 {
			continue
		}
		head := fields[0]
		// git is a write verb wholesale, but its read subcommands are allowed.
		if head == "git" {
			if !gitReadOnly(fields[1:]) {
				return "write command git " + strings.Join(fields[1:], " ")
			}
			continue
		}
		if writeVerbs[head] {
			return "write command " + head
		}
		// find can mutate through its action flags.
		if head == "find" {
			for _, f := range fields[1:] {
				switch f {
				case "-delete", "-exec", "-execdir", "-ok", "-okdir":
					return "write command find " + f
				}
			}
		}
		// In-place editors: sed/perl bundle -i into short options (-i, -pi).
		if head == "sed" || head == "perl" {
			for _, f := range fields[1:] {
				if strings.HasPrefix(f, "-") && !strings.HasPrefix(f, "--") && strings.Contains(f[1:], "i") {
					return "in-place edit " + head
				}
			}
		}
	}
	return ""
}

// writeVerbs are command words that mutate the filesystem or system state.
var writeVerbs = map[string]bool{
	// filesystem mutation
	"rm": true, "mv": true, "cp": true, "ln": true, "mkdir": true,
	"rmdir": true, "touch": true, "chmod": true, "chown": true, "chgrp": true,
	"truncate": true, "dd": true, "shred": true, "install": true,
	// package / system mutation
	"apt": true, "apt-get": true, "yum": true, "dnf": true, "brew": true,
	"npm": true, "yarn": true, "pnpm": true, "pip": true, "pip3": true,
	"cargo": true, "go": true, "make": true, "systemctl": true, "service": true,
	"mount": true, "umount": true, "mkfs": true, "shutdown": true, "reboot": true,
	// interpreters and shells: any of them can open files for writing, so a
	// planner must never be able to run one ("python -c ...", "node -e ...").
	"python": true, "python2": true, "python3": true, "py": true,
	"node": true, "nodejs": true, "ruby": true, "perl": true, "php": true,
	"lua": true, "bash": true, "sh": true, "zsh": true, "ksh": true,
	"dash": true, "fish": true, "pwsh": true, "powershell": true,
	"osascript": true, "eval": true,
	// wrappers that execute another command.
	"sudo": true, "env": true, "xargs": true,
}

// gitReadOnly reports whether a git subcommand is read-only. git is listed as
// a write verb wholesale, so read subcommands are re-admitted here. Subcommands
// that can write through flags (branch -D, tag -d, config key value, remote
// add, reflog expire) are checked flag by flag instead of admitted wholesale.
func gitReadOnly(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "log", "diff", "show", "status", "blame", "ls-files", "rev-parse",
		"describe", "grep", "shortlog", "show-ref", "cat-file", "merge-base",
		"name-rev", "whatchanged", "count-objects":
		return true
	case "branch":
		return branchReadOnly(args[1:])
	case "tag":
		return tagReadOnly(args[1:])
	case "remote":
		return remoteReadOnly(args[1:])
	case "config":
		return configReadOnly(args[1:])
	case "reflog":
		return reflogReadOnly(args[1:])
	}
	return false
}

// branchReadOnly: only listing flags are safe; a positional argument creates
// a branch and -d/-m/-c mutate.
func branchReadOnly(args []string) bool {
	allowed := map[string]bool{
		"-a": true, "-r": true, "-v": true, "-vv": true, "-l": true,
		"--list": true, "--all": true, "--remotes": true,
		"--show-current": true, "--no-color": true,
	}
	for _, a := range args {
		if !allowed[a] {
			return false
		}
	}
	return true
}

// tagReadOnly: listing flags only; a positional argument creates a tag and
// -d/-a/-m mutate.
func tagReadOnly(args []string) bool {
	allowed := map[string]bool{
		"-l": true, "--list": true, "-n": true, "--contains": true,
		"--merged": true, "--no-merged": true, "--points-at": true,
	}
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--sort="), strings.HasPrefix(a, "--format="),
			strings.HasPrefix(a, "--points-at="), strings.HasPrefix(a, "--contains="),
			strings.HasPrefix(a, "--merged="), strings.HasPrefix(a, "--no-merged="):
			continue
		}
		if !allowed[a] {
			return false
		}
	}
	return true
}

// remoteReadOnly: listing, show and get-url only; add/remove/rename/set-url/
// prune/update all write the repository config.
func remoteReadOnly(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "-v", "--verbose", "show", "get-url":
		return true
	}
	return false
}

// configReadOnly: reading flags only; "git config key value" writes.
func configReadOnly(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "--get", "--get-all", "--get-regexp", "--list", "-l",
		"--show-origin", "--show-scope":
		return true
	}
	return false
}

// reflogReadOnly: delete/expire mutate the reflog; everything else lists it.
func reflogReadOnly(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "delete", "expire", "drop":
		return false
	}
	return true
}
