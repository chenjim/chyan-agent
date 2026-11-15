// Inbound IM dispatch: authorized messages start an agent run and the reply
// goes back to the same chat (FR-7 / D35). Messages are queued per chat: a
// busy chat acknowledges immediately and queued messages are merged at the
// next turn boundary instead of running one full turn per message.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/channels"
	"chyan.tech/chyan/internal/core"
)

// PublicURLEnv names the externally reachable PWA base URL used in IM replies
// that were truncated. Empty falls back to the local daemon address.
const PublicURLEnv = "CHYAN_PUBLIC_URL"

// defaultPublicURL is the local PWA address used when PublicURLEnv is unset.
const defaultPublicURL = "http://127.0.0.1:31415"

// inboundState holds per-chat queues, rate limits and the recent message ids
// used for webhook redelivery dedupe.
type inboundState struct {
	server   *Server
	policies map[string]channels.InboundPolicy

	mu     sync.Mutex
	last   map[string]time.Time // chat key -> last accepted message
	seen   map[string]time.Time // message id -> first sighting
	queues map[string]*chatQueue
	closed bool
}

// chatQueue is one chat's FIFO of pending messages and its busy flag. Fields
// are guarded by inboundState.mu.
type chatQueue struct {
	key     string
	ch      chan queuedMessage
	pending int
	busy    bool
}

// queuedMessage carries the inbound timestamp so a turn can report how long
// its first message waited for a worker slot.
type queuedMessage struct {
	channels.TaggedMessage
	enqueued time.Time
}

// chatRef is where one inbound thread's messages are delivered. An empty
// ThreadID means the chat itself carries no topic/thread identity.
type chatRef struct {
	Channel  string `json:"channel"`
	ChatID   string `json:"chat_id"`
	ThreadID string `json:"thread_id,omitempty"`
}

const (
	// seenTTL is how long a provider message id is remembered for dedupe.
	seenTTL = 10 * time.Minute
	// maxInboundQueue bounds one chat's waiting messages; a flood is dropped
	// with a visible reply instead of growing goroutines/memory.
	maxInboundQueue = 8

	ackQueued    = "已收到，排队中（前面 %d 条）。"
	ackRunning   = "已收到，当前任务处理中，稍后合并处理。"
	ackQueueFull = "消息过多，已忽略（排队已满）。"
)

func newInboundState(s *Server, policies map[string]channels.InboundPolicy) *inboundState {
	return &inboundState{
		server: s, policies: policies,
		last: map[string]time.Time{}, seen: map[string]time.Time{},
		queues: map[string]*chatQueue{},
	}
}

// Dispatch validates one inbound message and enqueues it for the chat's
// worker. It returns immediately; the worker merges queued messages and runs
// them as one turn.
func (in *inboundState) Dispatch(ctx context.Context, tm channels.TaggedMessage) {
	policy, ok := in.policies[tm.Channel]
	if !ok || !policy.Enabled {
		return
	}
	msg := tm.Message
	if msg.FromBot {
		return // our own reply or another bot: never reply (loop guard)
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" && msg.ImageKey == "" {
		return
	}
	// Deterministic second-stage filter: plain chatter is bridged to SSE by
	// the caller but never costs a model turn.
	if policy.IntentFilter && !intentMatched(msg) {
		log.Printf("inbound: %s chat %s message has no agent intent, not run", tm.Channel, msg.ChatID)
		return
	}
	maxChars, minInterval := policy.Limits()
	if utf8.RuneCountInString(text) > maxChars {
		log.Printf("inbound: %s chat %s message over %d chars, dropped", tm.Channel, msg.ChatID, maxChars)
		return
	}
	if !policy.Authorized(msg.ChatID, msg.From) {
		log.Printf("inbound: %s chat %s from %s not in whitelist, dropped", tm.Channel, msg.ChatID, msg.From)
		return
	}
	key := threadKeyOf(tm)
	now := time.Now()
	in.mu.Lock()
	if in.closed {
		in.mu.Unlock()
		return
	}
	if msg.ID != "" {
		if _, dup := in.seen[msg.ID]; dup {
			in.mu.Unlock()
			return
		}
		in.seen[msg.ID] = now
		in.pruneLocked(now)
	}
	if last, ok := in.last[key]; ok && now.Sub(last) < minInterval {
		in.mu.Unlock()
		log.Printf("inbound: %s chat %s rate limited", tm.Channel, msg.ChatID)
		return
	}
	in.last[key] = now

	q := in.queues[key]
	startWorker := false
	if q == nil {
		q = &chatQueue{key: key, ch: make(chan queuedMessage, maxInboundQueue)}
		in.queues[key] = q
		startWorker = true
	}
	if q.pending >= maxInboundQueue {
		in.mu.Unlock()
		log.Printf("inbound: %s chat %s queue full, dropped", tm.Channel, msg.ChatID)
		in.reply(ctx, refOf(tm), ackQueueFull)
		return
	}
	ahead := q.pending
	q.pending++
	busy := q.busy
	in.mu.Unlock()

	// The buffer has room for maxInboundQueue messages and pending never
	// exceeds it, so this send cannot block.
	q.ch <- queuedMessage{TaggedMessage: tm, enqueued: now}
	if startWorker {
		go in.worker(ctx, q)
	}
	if busy {
		if ahead > 0 {
			in.reply(ctx, refOf(tm), fmt.Sprintf(ackQueued, ahead))
		} else {
			in.reply(ctx, refOf(tm), ackRunning)
		}
	}
}

// threadKeyOf is the per-thread routing identity: channel + chat, plus the
// provider thread id when the platform carries one. Each thread gets its own
// queue and its own worker session.
func threadKeyOf(tm channels.TaggedMessage) string {
	key := tm.Channel + ":" + tm.Message.ChatID
	if tid := strings.TrimSpace(tm.Message.ThreadID); tid != "" {
		key += ":t:" + tid
	}
	return key
}

// refOf captures the delivery target of one inbound message.
func refOf(tm channels.TaggedMessage) chatRef {
	return chatRef{
		Channel: tm.Channel, ChatID: tm.Message.ChatID,
		ThreadID: strings.TrimSpace(tm.Message.ThreadID),
	}
}

// intentMatched is the deterministic intent gate used when a channel enables
// IntentFilter: mentions, slash commands, questions and explicit request cues
// run; everything else stays conversation in the PWA only.
func intentMatched(msg channels.Message) bool {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return msg.ImageKey != "" // image-only messages are always for the agent
	}
	if strings.HasPrefix(text, "/") {
		return true
	}
	if strings.ContainsAny(text, "?？") {
		return true
	}
	if strings.HasPrefix(text, "@") || strings.Contains(text, "@_user") {
		return true
	}
	lower := strings.ToLower(text)
	for _, cue := range intentCues {
		if strings.Contains(lower, cue) {
			return true
		}
	}
	return false
}

// intentCues are explicit request markers (Chinese and English).
var intentCues = []string{
	"请", "帮", "麻烦", "写", "生成", "分析", "总结", "解释", "查询", "查一下",
	"搜索", "找一下", "修复", "优化", "实现", "部署", "测试", "检查", "对比",
	"推荐", "翻译", "计算", "整理", "复盘", "规划",
	"please", "help", "explain", "analyze", "analyse", "summarize", "summarise",
	"write", "create", "generate", "fix", "implement", "review", "check",
	"find", "search", "compare", "translate", "calculate", "build", "deploy",
}

// decisionAllowed reports whether an inbound message is trusted enough to
// resolve approval tickets: the channel must have inbound enabled and the
// chat/user must pass the same whitelist Dispatch applies. Bots are never
// trusted (a bot reply must not approve work).
func (in *inboundState) decisionAllowed(tm channels.TaggedMessage) bool {
	if in == nil || tm.Message.FromBot {
		return false
	}
	policy, ok := in.policies[tm.Channel]
	if !ok || !policy.Enabled {
		return false
	}
	return policy.Authorized(tm.Message.ChatID, tm.Message.From)
}

// pruneLocked drops expired dedupe entries so a long-lived daemon does not
// grow the map forever.
func (in *inboundState) pruneLocked(now time.Time) {
	if len(in.seen) < 256 {
		return
	}
	for id, at := range in.seen {
		if now.Sub(at) > seenTTL {
			delete(in.seen, id)
		}
	}
}

// worker drains one chat's queue until it is empty. Each batch is merged into
// one turn; the queue map entry disappears when no work is left.
func (in *inboundState) worker(ctx context.Context, q *chatQueue) {
	for {
		var first queuedMessage
		select {
		case <-ctx.Done():
			in.dropQueue(q.key)
			return
		case first = <-q.ch:
		}
		batch := []queuedMessage{first}
		drain := true
		for drain {
			select {
			case m := <-q.ch:
				batch = append(batch, m)
			default:
				drain = false
			}
		}

		in.mu.Lock()
		q.pending -= len(batch)
		q.busy = true
		in.mu.Unlock()

		in.processBatch(ctx, q.key, batch)

		in.mu.Lock()
		q.busy = false
		if q.pending <= 0 {
			delete(in.queues, q.key)
			in.mu.Unlock()
			return
		}
		in.mu.Unlock()
	}
}

// dropQueue removes a chat's queue after the daemon stops.
func (in *inboundState) dropQueue(key string) {
	in.mu.Lock()
	delete(in.queues, key)
	in.mu.Unlock()
}

// processBatch runs one merged turn for a chat and replies in place. The
// merged prompt keeps message order and collapses "/new" to a session reset.
func (in *inboundState) processBatch(ctx context.Context, key string, batch []queuedMessage) {
	ref := refOf(batch[0].TaggedMessage)
	channel, chatID := ref.Channel, ref.ChatID
	start := time.Now()
	// Remember where this thread's messages come from so background notices
	// (watchdog, task results) can be delivered straight back to it.
	in.saveSub(ctx, key, ref)
	newSession := false
	var texts []string
	var imageMsgs []channels.TaggedMessage
	for _, qm := range batch {
		tm := qm.TaggedMessage
		text := strings.TrimSpace(tm.Message.Text)
		if strings.EqualFold(text, "/new") {
			newSession = true
			continue
		}
		if text != "" {
			texts = append(texts, text)
		}
		if tm.Message.ImageKey != "" {
			imageMsgs = append(imageMsgs, tm)
		}
	}

	sessionID := ""
	if newSession {
		if err := in.server.store.SetKV(ctx, imSessionKey(key), nil); err != nil {
			log.Printf("inbound: reset session %s: %v", key, err)
		}
		if len(texts) == 0 && len(imageMsgs) == 0 {
			in.reply(ctx, ref, "已开启新会话。")
			return
		}
	} else if raw, err := in.server.store.GetKV(ctx, imSessionKey(key)); err == nil {
		sessionID = string(raw)
	}

	var images []app.ImageInput
	for _, tm := range imageMsgs {
		imgs, err := in.images(ctx, tm)
		if err != nil {
			log.Printf("inbound: %s chat %s image: %v", channel, chatID, err)
			in.reply(ctx, ref, "图片读取失败："+err.Error())
			return
		}
		images = append(images, imgs...)
	}
	if len(images) > core.MaxImagesPerTurn {
		images = images[:core.MaxImagesPerTurn]
	}
	prompt := strings.Join(texts, "\n")
	if strings.TrimSpace(prompt) == "" {
		prompt = "请描述这张图片。"
	}

	// IM runs share the daemon dispatch slots: an unbounded goroutine per
	// message would let a chat flood the LLM. The queue keeps waiting
	// messages, so blocking here is bounded by the chat's own queue.
	if !in.server.acquireTaskSlot() {
		return // daemon stopping: nothing will run this batch
	}
	defer in.server.releaseTaskSlot()
	runCtx, cancel := context.WithTimeout(ctx, in.server.taskTimeout)
	defer cancel()

	// P1-5: a thread's first request goes through the tool-less channel router,
	// which adds a dispatch instruction. Warm threads skip it: their session
	// already carries the conversation, so a second model turn would only
	// duplicate cost.
	if in.server.router != nil && sessionID == "" && len(texts) > 0 {
		if instr := in.server.router.route(runCtx, channel, prompt); instr != "" {
			prompt = prompt + "\n\n[router]\n" + instr
		}
	}

	reply, sid, err := in.server.app.RunOpts(runCtx, sessionID, prompt, app.RunOptions{Images: images})
	// Scheduling metric: queue wait, turn duration and merged batch size.
	in.server.publishMetric(map[string]any{
		"kind": "im.turn", "channel": channel, "chat_id": chatID, "thread": ref.ThreadID,
		"queued_ms": start.Sub(batch[0].enqueued).Milliseconds(),
		"turn_ms":   time.Since(start).Milliseconds(),
		"batch":     len(batch),
		"ok":        err == nil,
	})
	if err != nil {
		log.Printf("inbound: %s chat %s run failed: %v", channel, chatID, err)
		in.reply(ctx, ref, "执行失败："+err.Error())
		return
	}
	if sid != "" {
		if err := in.server.store.SetKV(ctx, imSessionKey(key), []byte(sid)); err != nil {
			log.Printf("inbound: session mapping %s: %v", key, err)
		}
		in.saveOwner(ctx, sid, ref)
	}
	text := strings.TrimSpace(core.FlattenText(reply))
	if text == "" {
		text = "（无输出）"
	}
	in.reply(ctx, ref, in.formatReply(channel, sid, text))
}

// formatReply enforces the channel's outbound length cap and appends the PWA
// link when the full answer was truncated.
func (in *inboundState) formatReply(channel, sessionID, text string) string {
	limit := channels.DefaultMaxReplyChars
	if p, ok := in.policies[channel]; ok {
		limit = p.ReplyLimit()
	}
	clamped, truncated := channels.ClampText(text, limit)
	if !truncated {
		return clamped
	}
	return clamped + "\n完整内容：" + pwaLink(sessionID)
}

// pwaLink returns the full-session PWA URL shown after a truncated reply.
func pwaLink(sessionID string) string {
	if sessionID == "" {
		return "(见 PWA 会话列表)"
	}
	base := strings.TrimRight(os.Getenv(PublicURLEnv), "/")
	if base == "" {
		base = defaultPublicURL
	}
	return base + "/#/sessions/" + sessionID
}

// imageFetcher is implemented by adapters that can download an inbound image
// attachment (feishu today). Channels without it reject image messages rather
// than dropping the attachment silently.
type imageFetcher interface {
	FetchImage(ctx context.Context, messageID, imageKey string) ([]byte, string, error)
}

// images resolves the inbound attachment, if any.
func (in *inboundState) images(ctx context.Context, tm channels.TaggedMessage) ([]app.ImageInput, error) {
	if tm.Message.ImageKey == "" {
		return nil, nil
	}
	reg := in.server.app.Channels()
	if reg == nil {
		return nil, fmt.Errorf("channel registry unavailable")
	}
	ch, ok := reg.Get(tm.Channel)
	if !ok {
		return nil, fmt.Errorf("channel %s not registered", tm.Channel)
	}
	fetcher, ok := ch.(imageFetcher)
	if !ok {
		return nil, fmt.Errorf("channel %s does not support inbound images yet", tm.Channel)
	}
	data, mime, err := fetcher.FetchImage(ctx, tm.Message.ID, tm.Message.ImageKey)
	if err != nil {
		return nil, err
	}
	return []app.ImageInput{{MIME: mime, Data: data}}, nil
}

// reply sends one outbound message to the originating thread; failures are
// logged, never fatal.
func (in *inboundState) reply(ctx context.Context, ref chatRef, text string) {
	in.server.sendRef(ctx, ref, text)
}

// sendRef delivers one text to a chat or thread; failures are logged.
func (s *Server) sendRef(ctx context.Context, ref chatRef, text string) {
	reg := s.app.Channels()
	if reg == nil {
		return
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := reg.Send(sctx, ref.Channel, channels.Outgoing{
		ChatID: ref.ChatID, ThreadID: ref.ThreadID, Text: text,
	}); err != nil {
		log.Printf("inbound: reply to %s/%s failed: %v", ref.Channel, ref.ChatID, err)
	}
}

// saveSub records where one thread's messages are delivered, enabling direct
// background delivery to that thread (P1 subscription routing).
func (in *inboundState) saveSub(ctx context.Context, key string, ref chatRef) {
	raw, err := json.Marshal(ref)
	if err != nil {
		return
	}
	if err := in.server.store.SetKV(ctx, imSubKey(key), raw); err != nil {
		log.Printf("inbound: subscription %s: %v", key, err)
	}
}

// saveOwner records which thread owns one session, so notices about that
// session go back to where the work was asked for.
func (in *inboundState) saveOwner(ctx context.Context, sessionID string, ref chatRef) {
	raw, err := json.Marshal(ref)
	if err != nil {
		return
	}
	if err := in.server.store.SetKV(ctx, imOwnerKey(sessionID), raw); err != nil {
		log.Printf("inbound: owner %s: %v", sessionID, err)
	}
}

// deliverThread sends text straight to the thread subscribed under key,
// bypassing the inbound queue. It reports whether a subscriber was delivered.
func (s *Server) deliverThread(ctx context.Context, key, text string) bool {
	return s.deliverRef(ctx, imSubKey(key), text)
}

// deliverSession sends text to the IM thread that owns sessionID.
func (s *Server) deliverSession(ctx context.Context, sessionID, text string) bool {
	if sessionID == "" {
		return false
	}
	return s.deliverRef(ctx, imOwnerKey(sessionID), text)
}

// deliverRef loads a stored chatRef under kvKey and sends text to it.
func (s *Server) deliverRef(ctx context.Context, kvKey, text string) bool {
	raw, err := s.store.GetKV(ctx, kvKey)
	if err != nil || len(raw) == 0 {
		return false
	}
	var ref chatRef
	if err := json.Unmarshal(raw, &ref); err != nil || ref.Channel == "" || ref.ChatID == "" {
		return false
	}
	s.sendRef(ctx, ref, text)
	return true
}

// imSessionKey is the store KV key mapping one thread to its session.
func imSessionKey(chatKey string) string { return "im:session:" + chatKey }

// imSubKey maps one thread to its delivery target.
func imSubKey(chatKey string) string { return "im:sub:" + chatKey }

// imOwnerKey maps one session to the thread it was requested from.
func imOwnerKey(sessionID string) string { return "im:owner:" + sessionID }
