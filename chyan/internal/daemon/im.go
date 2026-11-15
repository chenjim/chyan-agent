// IM inbound bridge: feishu webhook endpoint + channel updates to SSE.
package daemon

import (
	"context"
	"net/http"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/channels"
)

// webhookHandler is the feishu adapter's inbound endpoint (channels.Feishu
// implements it; the daemon mounts it).
type webhookHandler interface {
	HandleWebhook(w http.ResponseWriter, r *http.Request)
}

// handleFeishuWebhook mounts the feishu adapter's event callback
// (POST /api/v1/channels/feishu/webhook). The endpoint stays unauthenticated
// like the provider callback; the adapter verifies the signature.
func (s *Server) handleFeishuWebhook(w http.ResponseWriter, r *http.Request) {
	reg := s.app.Channels()
	if reg == nil {
		http.Error(w, "feishu channel not configured", http.StatusNotFound)
		return
	}
	ch, ok := reg.Get("feishu")
	if !ok {
		http.Error(w, "feishu channel not configured", http.StatusNotFound)
		return
	}
	h, ok := ch.(webhookHandler)
	if !ok {
		http.Error(w, "feishu channel has no webhook endpoint", http.StatusNotFound)
		return
	}
	h.HandleWebhook(w, r)
}

// bridgeChannelUpdates relays inbound IM messages to the SSE hub so
// TUI/PWA see them live (architecture 4.5). Runs until ctx ends.
func (s *Server) bridgeChannelUpdates(ctx context.Context) {
	reg := s.app.Channels()
	if reg == nil {
		return
	}
	// One stream for the whole loop: Updates() spawns a relay goroutine per
	// call, so re-calling it in the select would drop every second message.
	updates := reg.Updates()
	for {
		select {
		case <-ctx.Done():
			return
		case tm, ok := <-updates:
			if !ok {
				return
			}
			// Decision replies resolve approvals; they are not conversation.
			// Only the approval channel and a whitelisted sender may decide:
			// an unauthorized chat must never be able to approve agent work.
			if s.imApprover != nil && tm.Channel == s.imApprover.channel &&
				s.inbound.decisionAllowed(tm) && s.imApprover.HandleDecision(tm.Message.Text) {
				continue
			}
			s.hub.publish(Event{Name: api.EventMessage, Data: messageEvent(tm)})
			if s.inbound != nil {
				s.inbound.Dispatch(ctx, tm)
			}
		}
	}
}

// messageEvent is the SSE payload for one inbound IM message.
func messageEvent(tm channels.TaggedMessage) map[string]any {
	return map[string]any{
		"channel": tm.Channel, "chat_id": tm.Message.ChatID,
		"from": tm.Message.From, "text": tm.Message.Text,
		"at": tm.Message.At,
	}
}
