// Context budgeting and handoff compression (ported from the earlier Python
// implementation's context.py, with the handoff-note revision:
// compression folds old turns into a structured handoff note instead of a
// plain summary; the data layer is never rewritten — SQLite keeps the truth).
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"unicode/utf8"
)

// maxCompressRounds caps compression iterations per BuildMessages call to
// avoid pathological loops (legacy _MAX_COMPRESS_ROUNDS).
const maxCompressRounds = 16

// TokenCounter estimates token usage (llm.Counter satisfies this).
type TokenCounter interface {
	Count(text string) int
}

// ContextConfig holds budget thresholds. Zero-valued fields fall back to the
// legacy defaults (architecture appendix A).
type ContextConfig struct {
	// ContextLimit is the resolved model window in tokens (llm.ResolveContextLimit).
	ContextLimit int
	// ReservedTokens is held back for the reply (default 4096).
	ReservedTokens int
	// TriggerRatio is the soft threshold as a fraction of usable tokens
	// (default 0.7); hysteresis to TriggerRatio*CompressTargetRatio.
	TriggerRatio float64
	// TriggerBytes is the byte-size second trigger line (default 204800).
	TriggerBytes int
	// CompressTargetRatio is the post-compression watermark of the soft
	// threshold (default 0.8): 0.7 -> 0.56 is the hysteresis gap.
	CompressTargetRatio float64
	// MaxRecentTurns is the number of most recent turns protected from
	// compression (default 4).
	MaxRecentTurns int
	// SummaryRatio / SummaryMaxRatio / SummaryMaxTokens size summary
	// compression (defaults 0.3 / 0.15 / auto).
	SummaryRatio     float64
	SummaryMaxRatio  float64
	SummaryMaxTokens int
	// FoldToolBytes is the per-part size above which a tool result from an
	// EARLIER turn (before the last user message) is replaced by a head/tail
	// digest when building model-facing messages (default 2048; negative
	// disables folding). The store keeps full output either way.
	FoldToolBytes int
}

func (c *ContextConfig) withDefaults() ContextConfig {
	out := *c
	if out.ReservedTokens <= 0 {
		out.ReservedTokens = 4096
	}
	if out.TriggerRatio <= 0 {
		out.TriggerRatio = 0.7
	}
	if out.TriggerBytes <= 0 {
		out.TriggerBytes = 204800
	}
	if out.CompressTargetRatio <= 0 {
		out.CompressTargetRatio = 0.8
	}
	if out.MaxRecentTurns <= 0 {
		out.MaxRecentTurns = 4
	}
	if out.SummaryRatio <= 0 {
		out.SummaryRatio = 0.3
	}
	if out.SummaryMaxRatio <= 0 {
		out.SummaryMaxRatio = 0.15
	}
	if out.FoldToolBytes == 0 {
		out.FoldToolBytes = 2048
	}
	return out
}

// ContextManagerImpl is the concrete ContextManager (named to avoid clashing
// with the interface).
type ContextManagerImpl struct {
	mu sync.Mutex

	cfg        ContextConfig
	counter    TokenCounter
	compressor Compressor
	extractor  HandoffExtractor
	writer     HandoffWriter
	mirror     MemoryMirror
	sessionID  string

	systemPrompt string
	toolsTokens  int

	// note is the handoff prefix: structured sections plus a compressed
	// Summary. nil when nothing has been folded yet.
	note   *HandoffNote
	recent []Message
}

// ContextDeps are the optional collaborators of a context manager.
type ContextDeps struct {
	Counter    TokenCounter
	Compressor Compressor
	Extractor  HandoffExtractor
	Writer     HandoffWriter // optional; nil skips persistence
	Mirror     MemoryMirror  // optional; nil disables decision/correction mirroring
	SessionID  string
}

// NewContextManager builds a manager for one session. systemPrompt and
// toolDefs define the fixed cost; deps supplies counting/compression.
func NewContextManager(cfg ContextConfig, systemPrompt string, toolDefs []map[string]any, deps ContextDeps) *ContextManagerImpl {
	if deps.Counter == nil {
		deps.Counter = HeuristicCounter{}
	}
	if deps.Compressor == nil {
		deps.Compressor = TruncateCompressor{}
	}
	if deps.Extractor == nil {
		deps.Extractor = HeuristicExtractor{}
	}
	toolsTokens := 0
	if len(toolDefs) > 0 {
		if raw, err := json.Marshal(toolDefs); err == nil {
			toolsTokens = deps.Counter.Count(string(raw))
		}
	}
	return &ContextManagerImpl{
		cfg:          cfg.withDefaults(),
		counter:      deps.Counter,
		compressor:   deps.Compressor,
		extractor:    deps.Extractor,
		writer:       deps.Writer,
		mirror:       deps.Mirror,
		sessionID:    deps.SessionID,
		systemPrompt: systemPrompt,
		toolsTokens:  toolsTokens,
	}
}

// HeuristicCounter is a dependency-free token estimate: one token per CJK
// rune and per four ASCII bytes (always over-estimates slightly: compressing
// early is safe, overflowing the window is not). Satisfies TokenCounter.
type HeuristicCounter struct{}

// Count implements TokenCounter.
func (HeuristicCounter) Count(text string) int {
	if text == "" {
		return 0
	}
	ascii, other := 0, 0
	for _, r := range text {
		if r < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

// Append adds completed messages to recent history without trimming.
// Compression is deferred to BuildMessages (turn boundary), so long tool
// loops never lose mid-flight context.
func (m *ContextManagerImpl) Append(msgs ...Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recent = append(m.recent, msgs...)
	return nil
}

// BuildMessages assembles the LLM payload and may compress first. Compression
// only runs at this turn boundary and only when every tool call/result pair
// in recent is complete (never mid-tool, never mid-research).
func (m *ContextManagerImpl) BuildMessages(userInput string) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	userTokens := m.counter.Count(userInput)
	userBytes := len(userInput)
	if m.pairsComplete() {
		m.compressToFit(userTokens, userBytes)
	}

	out := make([]Message, 0, len(m.recent)+3)
	if m.systemPrompt != "" {
		out = append(out, textMessage(RoleSystem, m.systemPrompt))
	}
	if m.note != nil {
		out = append(out, textMessage(RoleSystem, renderNote(m.note)))
	}
	out = append(out, m.foldOldToolOutputs(m.recent)...)
	out = repairToolPairs(out)
	if userInput != "" {
		out = append(out, textMessage(RoleUser, userInput))
	}
	return out, nil
}

// repairToolPairs inserts a synthetic tool result after an assistant tool_call
// that has no matching result in the payload. A crash or cancel between the
// call and its result leaves exactly that hole, and OpenAI-compatible APIs
// reject the whole turn (400) when a tool_call is unanswered.
func repairToolPairs(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs)+2)
	for i := range msgs {
		out = append(out, msgs[i])
		if len(msgs[i].ToolCalls) == 0 {
			continue
		}
		answered := map[string]bool{}
		for j := i + 1; j < len(msgs) && msgs[j].Role == RoleTool; j++ {
			answered[msgs[j].ToolCallID] = true
		}
		for _, tc := range msgs[i].ToolCalls {
			if answered[tc.ID] {
				continue
			}
			out = append(out, Message{Role: RoleTool, ToolCallID: tc.ID,
				Content: []Part{{Type: "text", Text: "[tool error] interrupted before execution"}}})
		}
	}
	return out
}

// foldOldToolOutputs replaces oversized tool results from EARLIER turns with a
// head/tail digest before they enter the model-facing window. Everything
// after the last user message — the turn being executed right now — stays
// complete, because that is what the model is acting on. The store keeps the
// full output; this is a view, not a mutation, and folding here keeps the
// window small without paying an LLM call.
func (m *ContextManagerImpl) foldOldToolOutputs(msgs []Message) []Message {
	fold := m.cfg.FoldToolBytes
	if fold <= 0 {
		return msgs
	}
	last := -1
	for i := range msgs {
		if msgs[i].Role == RoleUser {
			last = i
		}
	}
	if last < 0 {
		return msgs
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := 0; i < last; i++ {
		if out[i].Role != RoleTool {
			continue
		}
		parts := make([]Part, len(out[i].Content))
		copy(parts, out[i].Content)
		for j, p := range parts {
			if p.Type == "text" && len(p.Text) > fold {
				parts[j].Text = foldToolText(p.Text, fold)
			}
		}
		out[i].Content = parts
	}
	return out
}

// foldToolText keeps the head and tail of an oversized tool output and labels
// what was dropped, on rune boundaries so CJK output stays intact.
func foldToolText(s string, fold int) string {
	const head, tail = 768, 256
	if omitted := len(s) - head - tail; omitted > 0 {
		return cutHead(s, head) + fmt.Sprintf("\n…[已折叠 %s：完整输出仍在会话存档，需要细节请重跑该工具]…\n", humanBytes(omitted)) + cutTail(s, tail)
	}
	return s
}

// humanBytes renders a byte count compactly (KB/MB).
func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// cutHead takes the first n bytes of s, backing off to a rune boundary.
func cutHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutTail takes the last n bytes of s, advancing to a rune boundary.
func cutTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s)-1 && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// Compact generates a handoff note now (manual /compact or caller-driven),
// folds recent history into it and returns the note. The full truth stays in
// the store; only the model-facing window is replaced by the note.
func (m *ContextManagerImpl) Compact(ctx context.Context) (HandoffNote, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	note, err := m.compactLocked(ctx)
	return note, err
}

func (m *ContextManagerImpl) compactLocked(ctx context.Context) (HandoffNote, error) {
	fold := m.foldMessages()
	note := HandoffNote{GeneratedAt: nowUTC()}
	if m.note != nil {
		// Carry forward decisions, corrections and lessons from the prior note.
		note.Intent = m.note.Intent
		note.Corrections = append([]string(nil), m.note.Corrections...)
		note.Decisions = append([]string(nil), m.note.Decisions...)
		note.References = append([]string(nil), m.note.References...)
		note.Evidence = append([]string(nil), m.note.Evidence...)
		note.Lessons = append([]string(nil), m.note.Lessons...)
	}
	var fresh HandoffNote
	if len(fold) > 0 {
		extracted, err := m.extractor.Extract(ctx, fold)
		if err != nil {
			return note, fmt.Errorf("handoff extract: %w", err)
		}
		fresh = extracted
		note.Intent = firstNonEmpty(extracted.Intent, note.Intent)
		note.Corrections = append(note.Corrections, extracted.Corrections...)
		note.Decisions = append(note.Decisions, extracted.Decisions...)
		note.Done = append(note.Done, extracted.Done...)
		note.Active = append(note.Active, extracted.Active...)
		note.Next = append(note.Next, extracted.Next...)
		note.References = append(note.References, extracted.References...)
		note.Lessons = append(note.Lessons, extracted.Lessons...)
		// Evidence is deterministic: raw artifact pointers and tool failures,
		// never LLM prose. A later conclusion must be traceable to these.
		note.Evidence = append(note.Evidence, extractEvidence(fold)...)
	}
	// Double insurance: mirror freshly extracted decisions/corrections into
	// long-term memory so a lost session cannot silently reopen settled
	// questions (architecture 4.1). Only new entries are mirrored —
	// carried-forward ones were stored at extraction. Pitfall lessons stay
	// note-only; their persistence is explicit or dream-extracted (4.3).
	if m.mirror != nil {
		for _, line := range fresh.Decisions {
			_ = m.mirror.Mirror(ctx, "decision", line)
		}
		for _, line := range fresh.Corrections {
			_ = m.mirror.Mirror(ctx, "correction", line)
		}
	}
	if len(fold) > 0 || (m.note != nil && m.note.Summary != "") {
		summary, err := m.compressor.Compress(ctx, fold, m.cfg.SummaryRatio)
		if err != nil {
			return note, fmt.Errorf("handoff summarize: %w", err)
		}
		note.Summary = joinNonEmpty(m.note.GetSummary(), summary)
	}
	note = dedupeNote(note)
	m.note = &note
	m.recent = nil
	if m.writer != nil {
		if err := m.writer.WriteHandoff(ctx, m.sessionID, note); err != nil {
			return note, fmt.Errorf("persist handoff: %w", err)
		}
	}
	return note, nil
}

// GetSummary exposes the summary section (nil-safe helper).
func (h *HandoffNote) GetSummary() string {
	if h == nil {
		return ""
	}
	return h.Summary
}

// Status reports current window usage.
func (m *ContextManagerImpl) Status() Budget {
	m.mu.Lock()
	defer m.mu.Unlock()
	usable := m.usableTokens()
	return Budget{
		ContextLimit:  m.cfg.ContextLimit,
		FixedCost:     m.fixedCost(),
		HardLimit:     usable,
		SoftLimit:     int(float64(usable) * m.cfg.TriggerRatio),
		HistoryTokens: m.historyTokensLocked(),
		HistoryBytes:  m.historyBytesLocked(true),
	}
}

// Reset clears handoff state and recent history.
func (m *ContextManagerImpl) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.note = nil
	m.recent = nil
}

// ------------------------------------------------------------------ budget

func (m *ContextManagerImpl) fixedCost() int {
	return m.counter.Count(m.systemPrompt) + m.toolsTokens
}

func (m *ContextManagerImpl) usableTokens() int {
	usable := m.cfg.ContextLimit - m.fixedCost() - m.cfg.ReservedTokens
	if usable < 0 {
		return 0
	}
	return usable
}

func (m *ContextManagerImpl) softLimitTokens() int {
	return int(float64(m.usableTokens()) * m.cfg.TriggerRatio)
}

func (m *ContextManagerImpl) targetTokens() int {
	return int(float64(m.softLimitTokens()) * m.cfg.CompressTargetRatio)
}

func (m *ContextManagerImpl) targetBytes() int {
	return int(float64(m.cfg.TriggerBytes) * m.cfg.CompressTargetRatio)
}

// summaryCapTokens bounds the note summary (legacy _summary_cap_tokens).
func (m *ContextManagerImpl) summaryCapTokens() int {
	if m.cfg.SummaryMaxTokens > 0 {
		return m.cfg.SummaryMaxTokens
	}
	cap := int(float64(m.usableTokens()) * m.cfg.SummaryMaxRatio)
	if cap < 1024 {
		return 1024
	}
	return cap
}

func (m *ContextManagerImpl) historyTokensLocked() int {
	total := 0
	if m.note != nil {
		total += m.counter.Count(renderNote(m.note))
	}
	total += countMessageTokens(m.counter, m.recent)
	return total
}

func (m *ContextManagerImpl) historyBytesLocked(includeNote bool) int {
	total := 0
	if includeNote && m.note != nil {
		total += len(renderNote(m.note))
	}
	for _, msg := range m.recent {
		for _, p := range msg.Content {
			total += len(p.Text)
		}
	}
	return total
}

// ------------------------------------------------------------- compression

// needCompress mirrors the legacy dual trigger: token soft threshold OR byte
// trigger line (either one fires).
func (m *ContextManagerImpl) needCompress(extraTokens, extraBytes int) bool {
	if m.historyTokensLocked()+extraTokens > m.softLimitTokens() {
		return true
	}
	return m.historyBytesLocked(true)+extraBytes > m.cfg.TriggerBytes
}

func (m *ContextManagerImpl) overTarget(extraTokens, extraBytes int) bool {
	if m.historyTokensLocked()+extraTokens > m.targetTokens() {
		return true
	}
	return m.historyBytesLocked(true)+extraBytes > m.targetBytes()
}

// compressToFit folds oldest turns into the handoff note until below the
// target watermark, protecting the most recent turns (legacy algorithm).
func (m *ContextManagerImpl) compressToFit(extraTokens, extraBytes int) {
	protected := protectedMessages(m.cfg.MaxRecentTurns)
	for i := 0; i < maxCompressRounds && m.needCompress(extraTokens, extraBytes); i++ {
		// Fold oldest turns in one batch (a single summary call), unless the
		// summary has already grown past the soft line — then shrink instead.
		hasSummary := m.note != nil && m.note.Summary != ""
		if len(m.recent) > protected && (!hasSummary || m.summaryTokens() < m.softLimitTokens()) {
			var moved []Message
			for len(m.recent) > protected && m.overTarget(extraTokens, extraBytes) {
				moved = append(moved, popOldestTurn(&m.recent)...)
			}
			if len(moved) > 0 {
				text, err := m.compressor.Compress(context.Background(), moved, m.cfg.SummaryRatio)
				if err != nil {
					log.Printf("core: compress turns failed: %v", err)
					break
				}
				m.appendSummary(text)
				m.shrinkSummary()
				continue
			}
		}
		if m.shrinkSummary() {
			continue
		}
		// Last resort: drop the note entirely only when that alone gets us
		// back under the line (minimize information loss).
		if m.note != nil && m.fitsWithoutNote(extraTokens, extraBytes) {
			log.Printf("core: dropping handoff note to protect recent turns")
			m.note = nil
			continue
		}
		break // cannot compress further; caller's window guard takes over
	}
}

func (m *ContextManagerImpl) summaryTokens() int {
	if m.note == nil {
		return 0
	}
	return m.counter.Count(m.note.Summary)
}

func (m *ContextManagerImpl) appendSummary(text string) {
	if text == "" {
		return
	}
	if m.note == nil {
		m.note = &HandoffNote{GeneratedAt: nowUTC()}
	}
	if m.note.Summary == "" {
		m.note.Summary = text
	} else {
		m.note.Summary += "\n" + text
	}
}

// shrinkSummary enforces the summary cap: re-compress when grossly over
// (compressor falls back to truncation), else FIFO-trim keeping the tail.
func (m *ContextManagerImpl) shrinkSummary() bool {
	if m.note == nil {
		return false
	}
	cap := m.summaryCapTokens()
	current := m.summaryTokens()
	if cap <= 0 || current <= cap {
		return false
	}
	if current > cap*2 {
		text, err := m.compressor.Compress(context.Background(),
			[]Message{textMessage(RoleAssistant, m.note.Summary)}, float64(cap)/float64(current))
		if err == nil && text != "" {
			m.note.Summary = text
		}
	}
	if m.summaryTokens() > cap {
		const ellipsis = "…"
		keep := cap - m.counter.Count(ellipsis)
		if keep < 1 {
			keep = 1
		}
		runes := []rune(m.note.Summary)
		if len(runes) > keep {
			runes = runes[len(runes)-keep:]
		}
		m.note.Summary = ellipsis + string(runes)
	}
	return true
}

func (m *ContextManagerImpl) fitsWithoutNote(extraTokens, extraBytes int) bool {
	tokens := countMessageTokens(m.counter, m.recent) + extraTokens
	bytes := m.historyBytesLocked(false) + extraBytes
	return tokens <= m.softLimitTokens() && bytes <= m.cfg.TriggerBytes
}

// pairsComplete reports whether tool calls and results are paired in recent —
// the gate for turn-boundary compression. Both directions matter: a result
// without a call and a call without a result both replay as invalid payloads.
func (m *ContextManagerImpl) pairsComplete() bool {
	seen := map[string]bool{}
	pending := map[string]bool{}
	for _, msg := range m.recent {
		if msg.Role == RoleTool {
			if msg.ToolCallID == "" || !seen[msg.ToolCallID] {
				return false
			}
			delete(pending, msg.ToolCallID)
			continue
		}
		for _, tc := range msg.ToolCalls {
			seen[tc.ID] = true
			pending[tc.ID] = true
		}
	}
	return len(pending) == 0
}

// foldMessages returns the messages to fold into the handoff note: the prior
// note (as synthetic context) followed by recent history.
func (m *ContextManagerImpl) foldMessages() []Message {
	if len(m.recent) == 0 {
		return nil
	}
	return m.recent
}

// ------------------------------------------------------------ message utils

func textMessage(role Role, text string) Message {
	return Message{Role: role, Content: []Part{{Type: "text", Text: text}}}
}

// messageText flattens one message for token counting.
func messageText(msg Message) string {
	var b strings.Builder
	b.WriteString(string(msg.Role))
	for _, p := range msg.Content {
		b.WriteString(p.Text)
	}
	for _, tc := range msg.ToolCalls {
		b.WriteString(tc.Name)
		if raw, err := json.Marshal(tc.Input); err == nil {
			b.Write(raw)
		}
	}
	return b.String()
}

// countMessageTokens sums message text plus per-message framing overhead.
func countMessageTokens(c TokenCounter, msgs []Message) int {
	total := 0
	for _, msg := range msgs {
		total += c.Count(messageText(msg)) + 4
	}
	return total
}

// protectedMessages is how many recent messages compression never touches:
// max_recent_turns complete turns, each a user+assistant pair.
func protectedMessages(maxRecentTurns int) int {
	n := maxRecentTurns
	if n < 1 {
		n = 1
	}
	return n * 2
}

// popOldestTurn removes the oldest turn from the front of msgs. A turn is a
// user message plus everything up to (excluding) the next user message — so
// assistant/tool/tool-result runs stay intact and tool call/result pairs are
// never split by compression.
func popOldestTurn(msgs *[]Message) []Message {
	slice := *msgs
	if len(slice) == 0 {
		return nil
	}
	// Skip a leading non-user message so grouping aligns on user boundaries.
	start := 0
	if slice[0].Role != RoleUser {
		for start < len(slice) && slice[start].Role != RoleUser {
			start++
		}
	}
	if start >= len(slice) {
		moved := slice
		*msgs = nil
		return moved
	}
	end := start + 1
	for end < len(slice) && slice[end].Role != RoleUser {
		end++
	}
	moved := slice[start:end]
	// Any leading non-user messages belong to the previous turn; fold them
	// into the moved batch so the summary sees contiguous history.
	moved = append(append([]Message(nil), slice[:start]...), moved...)
	*msgs = slice[end:]
	return moved
}

// renderNote formats a handoff note as an LLM-facing system message.
func renderNote(note *HandoffNote) string {
	if note == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("[对话交接 handoff]")
	section := func(title, body string) {
		if body != "" {
			b.WriteString("\n## ")
			b.WriteString(title)
			b.WriteString("\n")
			b.WriteString(body)
		}
	}
	section("Intent", note.Intent)
	section("Corrections", strings.Join(note.Corrections, "\n"))
	section("Decisions", strings.Join(note.Decisions, "\n"))
	section("Done", strings.Join(note.Done, "\n"))
	section("Active", strings.Join(note.Active, "\n"))
	section("Next", strings.Join(note.Next, "\n"))
	section("References", strings.Join(note.References, "\n"))
	section("Evidence", strings.Join(note.Evidence, "\n"))
	section("Lessons", strings.Join(note.Lessons, "\n"))
	section("Summary", note.Summary)
	return b.String()
}

func dedupeNote(n HandoffNote) HandoffNote {
	n.Corrections = dedupeStrings(n.Corrections)
	n.Decisions = dedupeStrings(n.Decisions)
	n.Done = dedupeStrings(n.Done)
	n.Active = dedupeStrings(n.Active)
	n.Next = dedupeStrings(n.Next)
	n.References = dedupeStrings(n.References)
	n.Evidence = dedupeStrings(n.Evidence)
	n.Lessons = dedupeStrings(n.Lessons)
	return n
}

// extractEvidence scans folded messages for artifact pointers and marked tool
// errors, pairing each with the tool name from the preceding call. Pure code:
// no model, no paraphrase, so the handoff carries anchors rather than claims.
func extractEvidence(msgs []Message) []string {
	const maxEntries = 40
	calls := map[string]string{}
	var out []string
	for _, msg := range msgs {
		if msg.Role == RoleAssistant {
			for _, tc := range msg.ToolCalls {
				calls[tc.ID] = tc.Name
			}
			continue
		}
		if msg.Role != RoleTool {
			continue
		}
		name := calls[msg.ToolCallID]
		if name == "" {
			name = "tool"
		}
		text := FlattenText(msg)
		if ref := artifactRef(text); ref != "" {
			out = append(out, name+" → "+ref)
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "[tool error] ") {
				out = append(out, name+" failed: "+clipLine(strings.TrimPrefix(line, "[tool error] "), 200))
				break
			}
		}
		if len(out) >= maxEntries {
			break
		}
	}
	return out
}

// artifactRef returns the first artifact pointer line in text, if any.
func artifactRef(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "[artifact: ") {
			ref := strings.TrimSuffix(strings.TrimPrefix(line, "[artifact: "), "]")
			if i := strings.Index(ref, " sha256:"); i >= 0 {
				ref = ref[:i]
			}
			return clipLine(ref, 300)
		}
	}
	return ""
}

func clipLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func joinNonEmpty(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}
