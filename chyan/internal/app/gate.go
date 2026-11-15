// Tool approval gating + audit logging before every tool execution.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
	"chyan.tech/chyan/internal/trace"
)

// ToolGate decides whether one tool call may proceed before it executes
// (architecture 4.2 approval flow). The permission engine (P3-2) supplies
// the production implementation; nil disables gating (P2 behaviour).
type ToolGate interface {
	// Ask blocks until approved or denied. Returns whether the operation may
	// proceed and a verdict string for the audit trail.
	Ask(ctx context.Context, req core.ApprovalRequest) (approved bool, verdict string, err error)
}

// SetGate installs the approval gate used before every tool execution.
func (a *App) SetGate(g ToolGate) { a.gate = g }

// gatedTool wraps a tool with approval gating + audit logging. Each run gets
// its own wrapper (session + agent permission set), so concurrent sub-agents
// never share permission state.
type gatedTool struct {
	inner core.Tool
	app   *App
	// session is the owning session for the audit trail.
	session string
	// perms is the agent-level permission subset (FR-5, may be nil): only
	// actions named here are constrained; others fall through to the default
	// gate flow (a subset narrows, it never widens).
	perms map[string]core.Effect
	// mode is the review mode string recorded in the audit trail.
	mode string
	// workDir is this run's workspace root, used to build diff previews for
	// the selected project rather than the daemon startup directory.
	workDir string
	// needsApproval reports whether this tool's call requires a gate check
	// (read-only tools pass through without asking).
	needsApproval bool
}

// Name implements core.Tool.
func (g *gatedTool) Name() string { return g.inner.Name() }

// previewRoot is the diff root for this run's workspace.
func (g *gatedTool) previewRoot() string {
	if g.workDir != "" {
		return g.workDir
	}
	return g.app.workDir
}

// Schema implements core.Tool.
func (g *gatedTool) Schema() map[string]any { return g.inner.Schema() }

// declaredEffect resolves the declared effect for an action ("*" is the
// catch-all key). ok=false means the action is not constrained.
func declaredEffect(perms map[string]core.Effect, action string) (core.Effect, bool) {
	if perms == nil {
		return "", false
	}
	if e, ok := perms[action]; ok {
		return e, true
	}
	if e, ok := perms["*"]; ok {
		return e, true
	}
	return "", false
}

// Execute gates the call then delegates to the inner tool, recording the
// outcome in the audit trail.
func (g *gatedTool) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	action := g.inner.Name()
	resource := resourceOf(call)
	// Agent-level permission subset (FR-5): only narrows, never widens.
	// deny hard-blocks even read-only tools; ask forces a gate check.
	eff, declared := declaredEffect(g.perms, action)
	if declared && eff == core.EffectDeny {
		g.app.audit(ctx, g.session, action, resource, "deny", false)
		return core.ToolResult{Err: trace.WithClass(
			fmt.Errorf("denied by agent permission: %s; this tool is not granted to the sub-agent", action),
			trace.ClassPermission)}
	}
	forceAsk := declared && eff == core.EffectAsk
	// Read-only tools skip the gate, but an explicit config rule still binds
	// them: deny hard-blocks and ask forces a review, same as the gate's own
	// checker path. An unmatched call keeps the read-only fast path (the
	// checker's ask default must not turn every lookup into an approval).
	if !g.needsApproval && !forceAsk && g.app.permChecker != nil {
		if rule, hit := g.app.permChecker.RuleEffect(action, resource); hit {
			switch rule {
			case core.EffectDeny:
				g.app.audit(ctx, g.session, action, resource, "deny", false)
				return core.ToolResult{Err: trace.WithClass(
					fmt.Errorf("denied by permission rule: %s %s; change the action or ask the user to update the rule",
						action, resource), trace.ClassPermission)}
			case core.EffectAsk:
				forceAsk = true
			}
		}
	}
	if !g.needsApproval && !forceAsk {
		return g.inner.Execute(ctx, call)
	}
	if g.app.gate == nil {
		return g.inner.Execute(ctx, call)
	}
	detail := detailOf(call)
	req := core.ApprovalRequest{
		Action:    action,
		Resource:  resource,
		Detail:    detail,
		Diff:      previewOf(g.previewRoot(), action, resource, call),
		SessionID: g.session,
	}

	ctx, span := trace.Start(ctx, g.app.tracer, trace.KindToolWaitApproval, action,
		map[string]any{"action": action, "resource": resource})
	approved, verdict, err := g.app.gate.Ask(ctx, req)
	if span != nil {
		span.SetAttr("approved", approved)
		span.SetAttr("verdict", verdict)
		span.End(err)
	}
	g.app.audit(ctx, g.session, action, resource, verdict, approved)
	if err != nil {
		return core.ToolResult{Err: fmt.Errorf("approval error: %w", err)}
	}
	if !approved {
		return core.ToolResult{Err: trace.WithClass(
			fmt.Errorf("operation denied by reviewer (%s): %s %s; do not retry until the user approves",
				verdict, action, resource), trace.ClassPermission)}
	}
	return g.inner.Execute(ctx, call)
}

// mergePerms narrows parent with child so the result is never wider than
// either (deny > ask > allow), matching core.PermSet.Intersect semantics for
// the action-level subset. Both maps are read-only inputs.
func mergePerms(parent, child map[string]core.Effect) map[string]core.Effect {
	if len(parent) == 0 {
		return child
	}
	if len(child) == 0 {
		return parent
	}
	out := make(map[string]core.Effect, len(parent)+len(child))
	for k, v := range parent {
		out[k] = v
	}
	for k, v := range child {
		if cur, ok := out[k]; ok {
			out[k] = tightenEffect(cur, v)
		} else {
			out[k] = v
		}
	}
	return out
}

// tightenEffect keeps the stricter of two effects (deny > ask > allow).
func tightenEffect(a, b core.Effect) core.Effect {
	rank := map[core.Effect]int{core.EffectAllow: 0, core.EffectAsk: 1, core.EffectDeny: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// audit records one gated operation for traceability (architecture 4.2).
func (a *App) audit(ctx context.Context, session, action, resource, verdict string, approved bool) {
	entry := store.AuditEntry{
		SessionID:  session,
		Action:     action,
		Resource:   resource,
		Effect:     "ask",
		ReviewMode: a.cfg.Review.Mode,
		Verdict:    verdict,
		Approved:   approved,
		Actor:      "user",
	}
	if err := a.store.AppendAudit(ctx, entry); err != nil {
		// Audit must not break the turn; log and continue.
		_ = err
	}
}

// resourceOf extracts the primary resource (path/command) from a tool call.
func resourceOf(call core.ToolCall) string {
	for _, key := range []string{"path", "command", "cmd", "file", "name"} {
		if v, ok := call.Input[key].(string); ok && v != "" {
			return v
		}
	}
	return call.Name
}

// detailOf renders a short human-readable summary of the call input.
func detailOf(call core.ToolCall) string {
	parts := make([]string, 0, len(call.Input))
	for k, v := range call.Input {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, " ")
}

// previewOf renders the pending change for the approver: a unified diff for
// file mutations (preview only, never written), the resource text otherwise
// (core.PreviewDiff has that semantic).
func previewOf(workDir, action, resource string, call core.ToolCall) string {
	before, after := previewBytes(workDir, action, call)
	return core.PreviewDiff(action, resource, before, after)
}

// previewBytes derives the before/after contents for the diff preview. Only
// edit/write/delete produce file contents; other actions fall through to
// core.PreviewDiff's command-text rendering.
func previewBytes(workDir, action string, call core.ToolCall) (before, after []byte) {
	path, _ := call.Input["path"].(string)
	if path == "" {
		return nil, nil
	}
	switch action {
	case "write", "delete":
	case "edit":
	default:
		return nil, nil
	}
	// Resolve like the fs tools do: relative paths live under the work dir.
	if !filepath.IsAbs(path) {
		path = filepath.Join(workDir, path)
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		cur = nil // missing file: diff against empty
	}
	switch action {
	case "write":
		content, _ := call.Input["content"].(string)
		if mode, _ := call.Input["mode"].(string); mode == "append" {
			return cur, append(append([]byte{}, cur...), content...)
		}
		return cur, []byte(content)
	case "edit":
		return cur, []byte(applyEditPreview(string(cur), call.Input))
	default: // delete
		return cur, nil
	}
}

// applyEditPreview mirrors tools/fs.go edit semantics (replace_all: first
// match by default) as a dry run; a missing old_string leaves the text
// unchanged so the preview diff is empty.
func applyEditPreview(text string, in map[string]any) string {
	oldS, _ := in["old_string"].(string)
	if oldS == "" {
		return text
	}
	newS, _ := in["new_string"].(string)
	replaceAll, _ := in["replace_all"].(bool)
	if !replaceAll && strings.Count(text, oldS) != 1 {
		return text
	}
	if replaceAll {
		return strings.ReplaceAll(text, oldS, newS)
	}
	return strings.Replace(text, oldS, newS, 1)
}

// readOnlyTools pass through the gate without asking. Beyond read-only
// lookups this includes two bookkeeping tools whose side effects never touch
// user files: "remember" writes chyan's own memory (superseded chains, dream
// dedupe) and "task" only dispatches a sub-agent — the child's own tool calls
// go through the gate again. Gating either stalls on an approval a
// non-interactive caller cannot answer (D24 denies it) and the work silently
// never happens. This map is the single approval decision for both the shared
// registry and per-run views (registryFor), so registerTools must stay in sync.
var readOnlyTools = map[string]bool{
	"read": true, "list": true, "search": true, "grep": true,
	"recall": true, "read_session": true, "remember": true, "task": true,
	// expand only re-reads spooled output chyan itself wrote.
	"expand": true,
}
