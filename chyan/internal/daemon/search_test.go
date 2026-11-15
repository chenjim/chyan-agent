package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

func TestSearchMessagesByContent(t *testing.T) {
	s, _, st := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	se := store.Session{ID: "ses-search", Title: "网络分析", Model: "m1", CreatedAt: now, UpdatedAt: now}
	if err := st.CreateSession(ctx, se); err != nil {
		t.Fatal(err)
	}
	parts := []core.Part{{Type: "text", Text: "用于防火墙策略或网络分析，建议直接查询 ASNBase / RIPE"}}
	raw, err := json.Marshal(struct {
		Content []core.Part `json:"content"`
	}{Content: parts})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendMessage(ctx, store.Message{
		ID: "m1", SessionID: se.ID, Role: "assistant",
		Content: raw, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// A different session without the term must not show up.
	other := store.Session{ID: "ses-other", Title: "无关会话", CreatedAt: now, UpdatedAt: now}
	if err := st.CreateSession(ctx, other); err != nil {
		t.Fatal(err)
	}

	w, req := doJSON(t, http.MethodGet, "/api/v1/search?q=防火墙", nil, "test-token")
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("search status = %d", w.Code)
	}
	var hits []api.SearchHit
	if err := json.Unmarshal(w.Body.Bytes(), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if hits[0].SessionID != se.ID || !strings.Contains(hits[0].Snippet, "防火墙") {
		t.Errorf("hit = %+v", hits[0])
	}
}

func TestSearchSkipsToolPayloads(t *testing.T) {
	s, _, st := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	se := store.Session{ID: "ses-tool", Title: "工具会话", CreatedAt: now, UpdatedAt: now}
	if err := st.CreateSession(ctx, se); err != nil {
		t.Fatal(err)
	}
	// A tool row carries model-facing JSON; searching it would show raw payload.
	raw, err := json.Marshal(struct {
		Content []core.Part `json:"content"`
	}{Content: []core.Part{{Type: "text",
		Text: `{"mode":"write","path":"/tmp/x","needle":"toolonly"}`}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendMessage(ctx, store.Message{
		ID: "mt", SessionID: se.ID, Role: string(core.RoleTool), Content: raw, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	w, req := doJSON(t, http.MethodGet, "/api/v1/search?q=toolonly", nil, "test-token")
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("search status = %d", w.Code)
	}
	var hits []api.SearchHit
	if err := json.Unmarshal(w.Body.Bytes(), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("tool payload must not be searchable: %+v", hits)
	}
}

func TestSearchEmptyAndMiss(t *testing.T) {
	s, _, _ := newTestServer(t)
	for _, q := range []string{"", "不存在的词"} {
		w, req := doJSON(t, http.MethodGet, "/api/v1/search?q="+q, nil, "test-token")
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		var hits []api.SearchHit
		if err := json.Unmarshal(w.Body.Bytes(), &hits); err != nil {
			t.Fatal(err)
		}
		if len(hits) != 0 {
			t.Errorf("q=%q hits=%d, want 0", q, len(hits))
		}
	}
}

func TestVisibleTextDropsReasoning(t *testing.T) {
	m := core.Message{Content: []core.Part{
		{Type: "reasoning", Text: "internal thinking"},
		{Type: "text", Text: "public answer"},
	}}
	if got := visibleText(m); got != "public answer" {
		t.Errorf("visibleText = %q", got)
	}
	m2 := core.Message{Content: []core.Part{{Type: "reasoning", Text: "only thinking"}}}
	if got := visibleText(m2); got != "" {
		t.Errorf("reasoning-only text = %q, want empty", got)
	}
}

func TestMatchSnippetWindow(t *testing.T) {
	text := strings.Repeat("前置内容", 30) + "核心 命中词 后续内容" + strings.Repeat("尾部内容", 30)
	snippet, ok := matchSnippet(text, "命中词")
	if !ok {
		t.Fatal("expected match")
	}
	if !strings.Contains(snippet, "命中词") {
		t.Errorf("snippet lost the match: %q", snippet)
	}
	if !strings.HasPrefix(snippet, "…") || !strings.HasSuffix(snippet, "…") {
		t.Errorf("expected window ellipses: %q", snippet)
	}
}
