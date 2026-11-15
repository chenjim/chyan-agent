// IM approval channel: push approval tickets to an IM chat and resolve them
// from inbound text decisions ("approve <id>" / "deny <id> [reason]").
package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"chyan.tech/chyan/internal/channels"
	"chyan.tech/chyan/internal/core"
)

// ApprovalChatIDEnv names the env var holding the chat that receives approval
// pushes. Empty = no IM push; approvals degrade to SSE/HTTP only.
const ApprovalChatIDEnv = "CHYAN_APPROVAL_CHAT_ID"

// IMApprover wraps an *ApprovalHub so ask-effect requests are pushed to an IM
// chat as text and resolved by replying a decision command. It implements
// core.Approver.
type IMApprover struct {
	hub         *ApprovalHub
	reg         *channels.Registry
	channel     string
	defaultChat string
}

// NewIMApprover wires a hub to a channel registry. chatID is the destination
// for approval pushes (may be empty: no IM push, SSE/HTTP only).
func NewIMApprover(hub *ApprovalHub, reg *channels.Registry, chatID string) *IMApprover {
	return &IMApprover{hub: hub, reg: reg, channel: "feishu", defaultChat: chatID}
}

// newIMApprover builds one from options: the gate must be the production hub
// and the app must have a channel registry. Returns nil otherwise (no IM
// approval channel; behavior unchanged).
func newIMApprover(opts Options) *IMApprover {
	hub, ok := opts.Gate.(*ApprovalHub)
	if !ok || opts.App == nil {
		return nil
	}
	reg := opts.App.Channels()
	if reg == nil {
		return nil
	}
	return NewIMApprover(hub, reg, os.Getenv(ApprovalChatIDEnv))
}

// Approve creates the approval ticket, pushes it to the IM chat, then blocks
// until a decision arrives or ctx ends (same semantics as ApprovalHub.Approve).
// A failed IM push is best-effort: the ticket stays resolvable over HTTP/TUI.
func (a *IMApprover) Approve(ctx context.Context, req core.ApprovalRequest) (bool, error) {
	id, wait, err := a.hub.Begin(req)
	if err != nil {
		return false, err
	}
	if a.defaultChat != "" {
		a.push(ctx, id, req)
	}
	return wait(ctx)
}

// push delivers the ticket as one IM text message; failures degrade silently.
func (a *IMApprover) push(ctx context.Context, id string, req core.ApprovalRequest) {
	text := approvalMessage(id, req)
	if err := a.reg.Send(ctx, a.channel, channels.Outgoing{ChatID: a.defaultChat, Text: text}); err != nil {
		log.Printf("imapprover: push approval %s degraded: %v", id, err)
	}
}

// HandleDecision parses an inbound IM text as a decision command
// ("approve <id> [reason]" / "deny <id> [reason]", case-insensitive, leading
// whitespace tolerated). A decision command is consumed and returns true (it
// is not conversation); anything else returns false for normal agent handling.
func (a *IMApprover) HandleDecision(text string) bool {
	decision, id, reason := parseDecision(text)
	if id == "" {
		return false
	}
	// Unknown/already-resolved ids stay consumed: a decision line is never
	// conversation, and nobody is waiting on it anymore.
	_ = a.hub.Resolve(id, decision, reason)
	return true
}

// parseDecision splits one decision command. id is empty for non-decision
// text.
func parseDecision(text string) (decision, id, reason string) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) < 2 {
		return "", "", ""
	}
	switch strings.ToLower(fields[0]) {
	case "approve":
		decision = "approve"
	case "deny":
		decision = "deny"
	default:
		return "", "", ""
	}
	id = fields[1]
	if len(fields) > 2 {
		reason = strings.Join(fields[2:], " ")
	}
	return decision, id, reason
}

// approvalMessage renders one ticket as the IM push body.
func approvalMessage(id string, req core.ApprovalRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[审批] %s %s\n", req.Action, req.Resource)
	if req.Detail != "" {
		fmt.Fprintf(&b, "detail: %s\n", req.Detail)
	}
	if req.Diff != "" {
		fmt.Fprintf(&b, "%s\n", req.Diff)
	}
	fmt.Fprintf(&b, "裁决: 回复 approve %s 或 deny %s [理由]", id, id)
	return b.String()
}

// wireIMApprover attaches the IM approval channel to a production-hub server
// (auto-wrap; Options stays unchanged). It degrades to nil (no IM pushes)
// when the gate is not the production hub or no channel registry exists.
func (s *Server) wireIMApprover(opts Options) {
	s.imApprover = newIMApprover(opts)
}

// compile-time check: the IM approver is the core human approver.
var _ core.Approver = (*IMApprover)(nil)
