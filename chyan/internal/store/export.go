// Session JSON export/import (envelope format v1). ExportSession snapshots one
// session (metadata, messages, audit trail) and ImportSession copies it into
// any Store as a fresh session with regenerated ids, never overwriting
// existing rows. The optional opencode importer maps documents written by
// `opencode session export` into the same envelope.
package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ExportFormatVersion is the envelope version written by ExportSession and
// accepted by ImportSession/ParseSessionExport.
const ExportFormatVersion = 1

// SessionExport is the portable envelope of one session: metadata, messages
// in creation order and the audit trail (oldest first, omitted when empty).
// JSON field names are snake_case.
type SessionExport struct {
	FormatVersion int               `json:"format_version"`
	ExportedAt    time.Time         `json:"exported_at"`
	Session       ExportedSession   `json:"session"`
	Messages      []ExportedMessage `json:"messages"`
	Audit         []ExportedAudit   `json:"audit,omitempty"`
}

// ExportedSession is the session metadata slice of an envelope (all fields of
// Session).
type ExportedSession struct {
	ID              string    `json:"id"`
	ParentID        string    `json:"parent_id"`
	Agent           string    `json:"agent"`
	Title           string    `json:"title"`
	Model           string    `json:"model"`
	CostUSD         float64   `json:"cost_usd"`
	TokensIn        int       `json:"tokens_in"`
	TokensOut       int       `json:"tokens_out"`
	CacheReadTokens int       `json:"cache_read_tokens"`
	ReasoningTokens int       `json:"reasoning_tokens"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ExportedMessage is one message row. Content is the raw parts-blob JSON
// (null when empty). Exported ids are informational only: ImportSession
// assigns fresh ids and rebinds session_id to the new session.
type ExportedMessage struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Truncated bool            `json:"truncated"`
	CreatedAt time.Time       `json:"created_at"`
}

// ExportedAudit is one audit entry. The store-assigned row id is
// database-local and is not exported; imported entries get fresh ids.
type ExportedAudit struct {
	Action     string    `json:"action"`
	Resource   string    `json:"resource"`
	Effect     string    `json:"effect"`
	ReviewMode string    `json:"review_mode"`
	Verdict    string    `json:"verdict"`
	Approved   bool      `json:"approved"`
	Actor      string    `json:"actor"`
	CreatedAt  time.Time `json:"created_at"`
}

// ExportSession snapshots one session with all its messages (creation order)
// and audit entries (oldest first) into a portable envelope.
func ExportSession(ctx context.Context, st Store, sessionID string) (*SessionExport, error) {
	sess, err := st.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	msgs, err := st.ListMessages(ctx, sessionID, 0, 0)
	if err != nil {
		return nil, err
	}
	audits, err := st.ListAudit(ctx, sessionID, 0)
	if err != nil {
		return nil, err
	}
	ex := &SessionExport{
		FormatVersion: ExportFormatVersion,
		ExportedAt:    time.Now().UTC(),
		Session: ExportedSession{
			ID: sess.ID, ParentID: sess.ParentID, Agent: sess.Agent,
			Title: sess.Title, Model: sess.Model,
			CostUSD: sess.CostUSD, TokensIn: sess.TokensIn, TokensOut: sess.TokensOut,
			CacheReadTokens: sess.CacheReadTokens, ReasoningTokens: sess.ReasoningTokens,
			CreatedAt: sess.CreatedAt, UpdatedAt: sess.UpdatedAt,
		},
		Messages: make([]ExportedMessage, 0, len(msgs)),
	}
	for _, m := range msgs {
		content, err := rawContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("store: export message %s: %w", m.ID, err)
		}
		ex.Messages = append(ex.Messages, ExportedMessage{
			ID: m.ID, SessionID: m.SessionID, Role: m.Role,
			Content: content, Truncated: m.Truncated, CreatedAt: m.CreatedAt,
		})
	}
	// ListAudit returns newest first; the envelope stores chronology.
	ex.Audit = make([]ExportedAudit, 0, len(audits))
	for i := len(audits) - 1; i >= 0; i-- {
		a := audits[i]
		ex.Audit = append(ex.Audit, ExportedAudit{
			Action: a.Action, Resource: a.Resource, Effect: a.Effect,
			ReviewMode: a.ReviewMode, Verdict: a.Verdict, Approved: a.Approved,
			Actor: a.Actor, CreatedAt: a.CreatedAt,
		})
	}
	return ex, nil
}

// ParseSessionExport decodes and validates an envelope produced by
// json.Marshal(SessionExport) or any ExportSession JSON output.
func ParseSessionExport(data []byte) (*SessionExport, error) {
	var ex SessionExport
	if err := json.Unmarshal(data, &ex); err != nil {
		return nil, fmt.Errorf("store: parse session export: %w", err)
	}
	if ex.FormatVersion != ExportFormatVersion {
		return nil, fmt.Errorf("store: parse session export: unsupported format_version %d", ex.FormatVersion)
	}
	if ex.Session.ID == "" {
		return nil, errors.New("store: parse session export: session id is empty")
	}
	return &ex, nil
}

// ImportSession inserts ex into st as a new session and returns the new
// session id. Session and message ids are regenerated (the envelope keeps the
// originals for traceability), messages are inserted in order and audit
// entries appended with fresh ids. Nothing is ever overwritten, so importing
// the same envelope twice is safe and yields two independent copies.
func ImportSession(ctx context.Context, st Store, ex *SessionExport) (string, error) {
	if ex == nil {
		return "", errors.New("store: import: nil export")
	}
	if ex.FormatVersion != ExportFormatVersion {
		return "", fmt.Errorf("store: import: unsupported format_version %d", ex.FormatVersion)
	}
	if ex.Session.ID == "" {
		return "", errors.New("store: import: session id is empty")
	}
	sessID, err := genRowID("ses")
	if err != nil {
		return "", err
	}
	if err := st.CreateSession(ctx, Session{
		ID: sessID, ParentID: ex.Session.ParentID, Agent: ex.Session.Agent,
		Title: ex.Session.Title, Model: ex.Session.Model,
		CostUSD: ex.Session.CostUSD, TokensIn: ex.Session.TokensIn, TokensOut: ex.Session.TokensOut,
		CacheReadTokens: ex.Session.CacheReadTokens, ReasoningTokens: ex.Session.ReasoningTokens,
		CreatedAt: ex.Session.CreatedAt, UpdatedAt: ex.Session.UpdatedAt,
	}); err != nil {
		return "", err
	}
	for _, em := range ex.Messages {
		id, err := genRowID("msg")
		if err != nil {
			return "", err
		}
		if err := st.AppendMessage(ctx, Message{
			ID: id, SessionID: sessID, Role: em.Role,
			Content: blobBytes(em.Content), Truncated: em.Truncated, CreatedAt: em.CreatedAt,
		}); err != nil {
			return "", err
		}
	}
	for _, ea := range ex.Audit {
		if err := st.AppendAudit(ctx, AuditEntry{
			SessionID: sessID, Action: ea.Action, Resource: ea.Resource,
			Effect: ea.Effect, ReviewMode: ea.ReviewMode, Verdict: ea.Verdict,
			Approved: ea.Approved, Actor: ea.Actor, CreatedAt: ea.CreatedAt,
		}); err != nil {
			return "", err
		}
	}
	return sessID, nil
}

// --- helpers ---

// rawContent validates the message content blob. Invalid JSON cannot be
// embedded as-is; rather than failing the whole export, the damaged bytes are
// salvaged as a single text part so the rest of the session still exports
// (legacy rows were byte-truncated into unparseable blobs).
func rawContent(content []byte) (json.RawMessage, error) {
	if len(content) == 0 {
		return json.RawMessage("null"), nil
	}
	if json.Valid(content) {
		return json.RawMessage(content), nil
	}
	salvage := map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(content)}},
	}
	raw, err := json.Marshal(salvage)
	if err != nil {
		return nil, errors.New("content is not valid JSON")
	}
	return json.RawMessage(raw), nil
}

// blobBytes inverts rawContent: null or absent content becomes nil, which the
// stores normalize to an empty blob.
func blobBytes(raw json.RawMessage) []byte {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	return raw
}

// genRowID generates a prefixed unique row id (same shape as internal/app ids).
func genRowID(prefix string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("store: generate id: %w", err)
	}
	return fmt.Sprintf("%s%016x", prefix, buf), nil
}

// --- opencode import (optional interop) ---

// ImportOpencodeSession imports a document written by `opencode session
// export` (shape verified against opencode v2.0.28) as a new session and
// returns the new session id. Mapping notes: the opencode model object becomes
// "providerID/id"; per-message cost/tokens and file snapshots have no store
// columns and are dropped (session totals are kept); an assistant message's
// tool parts expand into the chyan transcript shape — tool calls on the
// assistant message plus one follow-up tool message per result; opencode
// "synthetic" narration is imported as role "system".
func ImportOpencodeSession(ctx context.Context, st Store, data []byte) (string, error) {
	ex, err := parseOpencodeSession(data)
	if err != nil {
		return "", err
	}
	return ImportSession(ctx, st, ex)
}

// parseOpencodeSession converts one opencode export document into an envelope.
func parseOpencodeSession(data []byte) (*SessionExport, error) {
	var oc opencodeExport
	if err := json.Unmarshal(data, &oc); err != nil {
		return nil, fmt.Errorf("store: import opencode: %w", err)
	}
	if oc.Info.ID == "" {
		return nil, errors.New("store: import opencode: info.id is empty")
	}
	ex := &SessionExport{
		FormatVersion: ExportFormatVersion,
		ExportedAt:    time.Now().UTC(),
		Session: ExportedSession{
			ID: oc.Info.ID, Agent: oc.Info.Agent,
			Model:     opencodeModelString(oc.Info.Model),
			CostUSD:   oc.Info.Cost,
			TokensIn:  oc.Info.Tokens.Input,
			TokensOut: oc.Info.Tokens.Output,
			CreatedAt: ocMillis(oc.Info.Time.Created),
			UpdatedAt: ocMillis(oc.Info.Time.Updated),
			Title:     oc.Info.Title,
		},
		Messages: []ExportedMessage{},
	}
	var prev time.Time
	for _, om := range oc.Messages {
		built, err := opencodeMessages(om, oc.Info.ID)
		if err != nil {
			return nil, err
		}
		for i := range built {
			built[i].CreatedAt = nextStamp(built[i].CreatedAt, prev)
			prev = built[i].CreatedAt
		}
		ex.Messages = append(ex.Messages, built...)
	}
	return ex, nil
}

// opencodeMessages maps one opencode message to one or more envelope rows:
// plain messages map 1:1, assistant tool parts expand into a tool message
// each so tool-call/result pairs stay adjacent as in a native chyan session.
func opencodeMessages(om opencodeMessage, sessionID string) ([]ExportedMessage, error) {
	at := ocMillis(om.Time.Created)
	switch om.Type {
	case "user", "system", "synthetic":
		if len(om.Content) > 0 {
			return nil, fmt.Errorf("store: import opencode: %s message %s has content parts", om.Type, om.ID)
		}
		role := om.Type
		if role == "synthetic" {
			role = "system"
		}
		content, err := marshalBlob(msgBlob{Content: []blobPart{{Type: "text", Text: om.Text}}})
		if err != nil {
			return nil, err
		}
		return []ExportedMessage{{
			ID: om.ID, SessionID: sessionID, Role: role,
			Content: content, CreatedAt: at,
		}}, nil
	case "assistant":
		var parts []blobPart
		var calls []blobToolCall
		var results []ExportedMessage
		for _, p := range om.Content {
			switch p.Type {
			case "text":
				parts = append(parts, blobPart{Type: "text", Text: p.Text})
			case "reasoning":
				parts = append(parts, blobPart{Type: "reasoning", Text: p.Text})
			case "tool":
				if p.State == nil {
					return nil, fmt.Errorf("store: import opencode: tool part %s has no state", p.ID)
				}
				calls = append(calls, blobToolCall{ID: p.ID, Name: p.Name, Input: p.State.Input})
				result, err := opencodeToolResult(om, p, sessionID)
				if err != nil {
					return nil, err
				}
				results = append(results, result)
			default:
				return nil, fmt.Errorf("store: import opencode: unsupported part type %q", p.Type)
			}
		}
		content, err := marshalBlob(msgBlob{Content: parts, ToolCalls: calls})
		if err != nil {
			return nil, err
		}
		return append([]ExportedMessage{{
			ID: om.ID, SessionID: sessionID, Role: "assistant",
			Content: content, CreatedAt: at,
		}}, results...), nil
	default:
		return nil, fmt.Errorf("store: import opencode: unsupported message type %q", om.Type)
	}
}

// opencodeToolResult maps one opencode tool part's state to a chyan tool
// result message (role "tool", tool_call_id set, result texts as parts).
func opencodeToolResult(om opencodeMessage, p opencodePart, sessionID string) (ExportedMessage, error) {
	var parts []blobPart
	for _, c := range p.State.Content {
		parts = append(parts, blobPart{Type: "text", Text: c.Text})
	}
	if p.State.Error != nil {
		text := p.State.Error.Message
		if text == "" {
			text = p.State.Error.Type
		}
		parts = append(parts, blobPart{Type: "text", Text: text})
	}
	content, err := marshalBlob(msgBlob{Content: parts, ToolCallID: p.ID})
	if err != nil {
		return ExportedMessage{}, err
	}
	at := ocMillis(om.Time.Created)
	if p.Time != nil && p.Time.Created != 0 {
		at = ocMillis(p.Time.Created)
	}
	return ExportedMessage{
		ID: p.ID, SessionID: sessionID, Role: "tool",
		Content: content, CreatedAt: at,
	}, nil
}

// msgBlob mirrors the persisted message shape written by internal/app
// (content/tool_calls/tool_call_id). Part and tool-call JSON keys match
// encoding/json's default field names of core.Part/core.ToolCall so imported
// rows replay through internal/app unchanged.
type msgBlob struct {
	Content    []blobPart     `json:"content"`
	ToolCalls  []blobToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type blobPart struct {
	Type string `json:"Type"`
	Text string `json:"Text"`
}

type blobToolCall struct {
	ID    string         `json:"ID"`
	Name  string         `json:"Name"`
	Input map[string]any `json:"Input"`
}

func marshalBlob(b msgBlob) (json.RawMessage, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("store: import opencode: marshal content: %w", err)
	}
	return json.RawMessage(raw), nil
}

// opencodeModelString maps the opencode model object to chyan's free-form
// model string.
func opencodeModelString(m opencodeModel) string {
	if m.ProviderID == "" {
		return m.ID
	}
	return m.ProviderID + "/" + m.ID
}

// ocMillis converts an opencode epoch-millisecond timestamp; 0 means unset.
func ocMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// nextStamp normalizes an imported message timestamp so document order
// survives ListMessages' (created_at, id) sort: the result is strictly
// greater than prev, and its nanosecond last digit is nonzero. The latter
// matters because RFC3339Nano trims trailing zeros and trimmed fractions
// misorder in SQLite's text sort ("...8Z" sorts after "...81Z"), which would
// put tool results before their assistant message. Every timestamp stays
// within 10ns of its opencode millisecond source.
func nextStamp(t, prev time.Time) time.Time {
	if !prev.IsZero() && !t.After(prev) {
		t = prev.Add(time.Nanosecond)
	}
	for !t.IsZero() && t.Nanosecond()%10 == 0 {
		t = t.Add(time.Nanosecond)
	}
	return t
}

// opencodeExport mirrors the output of `opencode session export` (verified
// against opencode v2.0.28): {"info": {...}, "messages": [...]}. Timestamps
// are epoch milliseconds; fields not listed here are ignored.
type opencodeExport struct {
	Info     opencodeInfo      `json:"info"`
	Messages []opencodeMessage `json:"messages"`
}

type opencodeInfo struct {
	ID     string         `json:"id"`
	Agent  string         `json:"agent"`
	Model  opencodeModel  `json:"model"`
	Cost   float64        `json:"cost"`
	Tokens opencodeTokens `json:"tokens"`
	Time   opencodeTime   `json:"time"`
	Title  string         `json:"title"`
}

type opencodeModel struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
}

type opencodeTokens struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

type opencodeTime struct {
	Created int64 `json:"created"` // epoch milliseconds
	Updated int64 `json:"updated"`
}

type opencodeMessage struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"` // user | assistant | system | synthetic
	Text    string         `json:"text"`
	Time    opencodeStamp  `json:"time"`
	Content []opencodePart `json:"content"`
}

type opencodeStamp struct {
	Created   int64 `json:"created"` // epoch milliseconds
	Completed int64 `json:"completed"`
}

type opencodePart struct {
	Type  string             `json:"type"` // text | reasoning | tool
	Text  string             `json:"text"`
	ID    string             `json:"id"`
	Name  string             `json:"name"`
	State *opencodeToolState `json:"state"`
	Time  *opencodeStamp     `json:"time"`
}

type opencodeToolState struct {
	Content []opencodeText `json:"content"`
	Input   map[string]any `json:"input"`
	Error   *opencodeError `json:"error"`
}

type opencodeText struct {
	Text string `json:"text"`
}

type opencodeError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
