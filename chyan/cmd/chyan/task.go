// task.go — drive one daemon task from the CLI: subscribe to SSE first, create
// the task, stream deltas/progress, resolve approval tickets, and fall back to
// polling when the stream breaks. The final reply always comes from the task
// row, so a late subscriber still reports the truth.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/tui"
)

// taskHooks receives the live task lifecycle. onCreated fires once the daemon
// accepted the task (the interrupt handler needs its id).
type taskHooks struct {
	onText     func(string)
	onProgress func(tui.Event)
	onCreated  func(api.Task)
	approve    func(ctx context.Context, t api.ApprovalTicket) bool
}

// runDaemonTask creates one task and blocks until it reaches a terminal state.
// It returns the terminal task plus its failure (interrupted maps to
// core.ErrInterrupted so the CLI can exit 130).
func runDaemonTask(ctx context.Context, c *tui.Client, req api.CreateTaskRequest, h taskHooks) (api.Task, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	events := make(chan tui.Event, 512)
	ready := make(chan struct{}, 1)
	streamErr := make(chan error, 1)
	go func() {
		streamErr <- c.EventsWithReady(runCtx, events, func() {
			select {
			case ready <- struct{}{}:
			default:
			}
		})
	}()
	// Subscribe before creating the task so no early token deltas are lost.
	select {
	case <-ready:
	case err := <-streamErr:
		return api.Task{}, fmt.Errorf("事件流不可用: %w", err)
	case <-time.After(5 * time.Second):
		// Slow stream handshake: polling below still reports the final reply.
	case <-ctx.Done():
		return api.Task{}, ctx.Err()
	}

	task, err := c.CreateTask(runCtx, req)
	if err != nil {
		return api.Task{}, err
	}
	if h.onCreated != nil {
		h.onCreated(task)
	}
	// A previous turn (or another client) may already be waiting on us. The
	// SSE subscription is live before this snapshot, so a ticket can appear in
	// both places; track ids so the human is asked once.
	handled := map[string]bool{}
	if pending, err := c.ListApprovals(runCtx); err == nil {
		for _, tk := range pending {
			if tk.SessionID != task.SessionID {
				continue
			}
			handled[tk.ID] = true
			if err := settleApproval(runCtx, c, tk, h.approve); err != nil {
				_ = cancelTask(c, task.ID)
				return task, err
			}
		}
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case ev := <-events:
			id, _ := ev.Data["id"].(string)
			switch ev.Name {
			case api.EventTaskOutput:
				// Terminal snapshots carry the accumulated reply, not a delta;
				// printing them would duplicate the streamed text.
				status, _ := ev.Data["status"].(string)
				if id == task.ID && !terminalStatus(status) && h.onText != nil {
					h.onText(ev.TaskOutput())
				}
			case api.EventTaskProgress:
				if id == task.ID && h.onProgress != nil {
					h.onProgress(ev)
				}
			case api.EventTaskDone:
				if id != task.ID {
					continue
				}
				done, err := ev.Task()
				if err != nil {
					return task, err
				}
				return done, terminalError(done)
			case api.EventApprovalNew:
				tk, err := ticketOf(ev)
				if err != nil || tk.SessionID != task.SessionID || handled[tk.ID] {
					continue
				}
				handled[tk.ID] = true
				if err := settleApproval(runCtx, c, tk, h.approve); err != nil {
					_ = cancelTask(c, task.ID)
					return task, err
				}
			case api.EventApprovalDone:
				// Already decided elsewhere: a queued "requested" frame must
				// not trigger a second prompt.
				if id, _ := ev.Data["id"].(string); id != "" {
					handled[id] = true
				}
			}
		case err := <-streamErr:
			// Degraded: keep polling the task row until it is terminal.
			_ = err
			streamErr = nil
		case <-ticker.C:
			got, err := c.GetTask(runCtx, task.ID)
			if err == nil && terminalStatus(got.Status) {
				return got, terminalError(got)
			}
		case <-ctx.Done():
			// The client is going away; do not leave the turn running headless.
			_ = cancelTask(c, task.ID)
			return task, ctx.Err()
		}
	}
}

// cancelTask asks the daemon to interrupt a task (best effort).
func cancelTask(c *tui.Client, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.CancelTask(ctx, id)
	return err
}

// settleApproval asks the human and posts the decision. A 409/404 means the
// ticket was already resolved (another client), which is not an error here.
func settleApproval(ctx context.Context, c *tui.Client, tk api.ApprovalTicket, approve func(context.Context, api.ApprovalTicket) bool) error {
	decision := "deny"
	if approve != nil && approve(ctx, tk) {
		decision = "approve"
	}
	err := c.DecideApproval(ctx, tk.ID, api.ApprovalDecision{Decision: decision})
	if err == nil || isResolvedConflict(err) {
		return nil
	}
	return fmt.Errorf("提交审批结果失败: %w", err)
}

// isResolvedConflict recognizes a ticket that someone else already decided.
func isResolvedConflict(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "daemon 409") || strings.Contains(msg, "daemon 404")
}

// ticketOf decodes one approval ticket from an SSE payload.
func ticketOf(ev tui.Event) (api.ApprovalTicket, error) {
	if ev.Data == nil {
		return api.ApprovalTicket{}, errors.New("审批事件无负载")
	}
	raw, err := json.Marshal(ev.Data)
	if err != nil {
		return api.ApprovalTicket{}, err
	}
	var tk api.ApprovalTicket
	if err := json.Unmarshal(raw, &tk); err != nil {
		return api.ApprovalTicket{}, err
	}
	return tk, nil
}

// terminalStatus reports whether the task reached a final state.
func terminalStatus(s string) bool {
	return s == "done" || s == "failed" || s == "interrupted"
}

// terminalError maps a terminal task onto the CLI error contract.
func terminalError(t api.Task) error {
	switch t.Status {
	case "interrupted":
		return core.ErrInterrupted
	case "failed":
		msg := strings.TrimSpace(t.Err)
		if msg == "" {
			msg = "任务失败"
		}
		return errors.New(msg)
	}
	return nil
}

// approvalPrompt asks y/n on the terminal. auto approves without asking
// (--auto); a closed or non-interactive stdin denies, matching the old run.
type approvalPrompt struct {
	in   *bufio.Reader
	out  io.Writer
	auto bool
}

// newApprovalPrompt builds the prompt over process stdio (in is shared across
// tickets so piped y/n lines are not swallowed by a fresh buffer).
func newApprovalPrompt(in io.Reader, out io.Writer, auto bool) *approvalPrompt {
	return &approvalPrompt{in: bufio.NewReader(in), out: out, auto: auto}
}

// decide prints the request and reads one decision line.
func (p *approvalPrompt) decide(ctx context.Context, t api.ApprovalTicket) bool {
	if p.auto {
		return true
	}
	fmt.Fprintf(p.out, "审批请求：%s %s\n", t.Action, t.Resource)
	if t.Diff != "" {
		fmt.Fprintln(p.out, t.Diff)
	}
	fmt.Fprint(p.out, "批准？[y/N] ")
	line := make(chan string, 1)
	go func() {
		s, _ := p.in.ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case ans := <-line:
		return ans == "y" || ans == "Y" || strings.EqualFold(ans, "yes")
	case <-ctx.Done():
		return false
	}
}
