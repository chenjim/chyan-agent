// Package deliver routes job output to its target channel ("feishu:oc_x",
// "telegram:123") so scheduled jobs can push to IM (P4 exit: hermes cron
// delivery migrates intact).
package deliver

import (
	"context"
	"fmt"
	"strings"

	"chyan.tech/chyan/internal/channels"
)

// Router delivers text to a named channel in a channels.Registry.
type Router struct {
	reg *channels.Registry
}

// NewRouter wires the channel registry (nil disables delivery).
func NewRouter(reg *channels.Registry) *Router {
	return &Router{reg: reg}
}

// Route sends text to one delivery target. The target form is
// "<channel>[:<chatID>]"; without a chat id the channel default is used
// (feishuConfig.ReceiveIDType), matching hermes deliver strings like
// "feishu" or "feishu:oc_8b6b...".
func (r *Router) Route(ctx context.Context, target, text string) error {
	if r == nil || r.reg == nil {
		return fmt.Errorf("deliver: no channels configured")
	}
	name, chatID, _ := strings.Cut(strings.TrimSpace(target), ":")
	if name == "" {
		return fmt.Errorf("deliver: empty target")
	}
	// IM output is a notification, not a transcript: cap it at the channel's
	// configured reply length so cron delivery never floods a phone chat.
	limit := channels.DefaultMaxReplyChars
	if p, ok := r.reg.InboundPolicies()[name]; ok {
		limit = p.ReplyLimit()
	}
	text, _ = channels.ClampText(text, limit)
	return r.reg.Send(ctx, name, channels.Outgoing{ChatID: chatID, Text: text})
}
