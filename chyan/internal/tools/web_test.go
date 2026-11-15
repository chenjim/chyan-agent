// web_test.go: web_fetch and web_search tests over httptest servers and
// fake providers (no real network).
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"chyan.tech/chyan/internal/core"
)

// runWeb executes a tool and decodes the JSON payload.
func runWeb(t *testing.T, tool core.Tool, in map[string]any) map[string]any {
	t.Helper()
	res := tool.Execute(context.Background(), core.ToolCall{ID: "1", Name: tool.Name(), Input: in})
	out := map[string]any{}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("result not JSON: %v: %s", err, res.Content)
	}
	return out
}

// newTestFetch builds a fetch tool wired to a loopback httptest server
// (SSRF guard disabled on the test seam only).
func newTestFetch(timeout time.Duration, maxOut int) *WebFetchTool {
	tool := NewWebFetchTool(timeout, maxOut)
	tool.allowPrivate = true
	return tool
}

func TestWebFetchHTMLToText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!doctype html><html><head><style>h1{color:red}</style>
<script>var x = 1;</script></head><body><h1>Hello &amp; World</h1>
<p>para   one</p></body></html>`))
	}))
	defer srv.Close()

	tool := newTestFetch(5*time.Second, 20000)
	out := runWeb(t, tool, map[string]any{"url": srv.URL})
	if out["success"] != true {
		t.Fatalf("want success, got %v", out)
	}
	if out["status"] != float64(200) {
		t.Errorf("status = %v", out["status"])
	}
	text := out["output"].(string)
	if strings.Contains(text, "color:red") || strings.Contains(text, "var x") {
		t.Errorf("script/style not stripped: %q", text)
	}
	if !strings.Contains(text, "Hello & World") {
		t.Errorf("entity not decoded: %q", text)
	}
	if strings.Contains(text, "<h1>") {
		t.Errorf("tags not stripped: %q", text)
	}
}

func TestWebFetchPlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("raw <b>keep</b>"))
	}))
	defer srv.Close()

	tool := newTestFetch(5*time.Second, 20000)
	out := runWeb(t, tool, map[string]any{"url": srv.URL})
	if out["success"] != true {
		t.Fatalf("want success, got %v", out)
	}
	if out["output"] != "raw <b>keep</b>" {
		t.Errorf("plain body altered: %q", out["output"])
	}
}

func TestWebFetchTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(strings.Repeat("x", 500)))
	}))
	defer srv.Close()

	// body-level truncation via max_bytes
	tool := newTestFetch(5*time.Second, 20000)
	out := runWeb(t, tool, map[string]any{"url": srv.URL, "max_bytes": 100})
	if out["success"] != true {
		t.Fatalf("want success, got %v", out)
	}
	if out["truncated"] != true {
		t.Errorf("want truncated, got %v", out)
	}
	if n := len(out["output"].(string)); n > 100 {
		t.Errorf("body not capped: %d bytes", n)
	}

	// tool-output truncation via maxOut
	tool = newTestFetch(5*time.Second, 50)
	out = runWeb(t, tool, map[string]any{"url": srv.URL})
	if out["output_truncated"] != true {
		t.Errorf("want output_truncated, got %v", out)
	}
	if n := len(out["output"].(string)); n > 100 {
		t.Errorf("output not capped: %d bytes", n)
	}
}

func TestWebFetchTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	tool := newTestFetch(300*time.Millisecond, 20000)
	out := runWeb(t, tool, map[string]any{"url": srv.URL})
	if out["success"] == true {
		t.Fatal("want timeout failure")
	}
	if out["timed_out"] != true {
		t.Errorf("timed_out = %v", out["timed_out"])
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "timed out") {
		t.Errorf("error = %q", msg)
	}
}

func TestWebFetchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	tool := newTestFetch(5*time.Second, 20000)
	out := runWeb(t, tool, map[string]any{"url": srv.URL})
	if out["success"] == true {
		t.Error("5xx must not be success")
	}
	if out["status"] != float64(500) {
		t.Errorf("status = %v", out["status"])
	}
}

func TestWebFetchSSRFGuard(t *testing.T) {
	// guard active by default: loopback httptest targets must be refused
	tool := NewWebFetchTool(5*time.Second, 20000)
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"loopback ip", "http://127.0.0.1:8080/", "ssrf guard"},
		{"localhost", "http://localhost:8080/", "ssrf guard"},
		{"rfc1918 a", "http://10.1.2.3/", "ssrf guard"},
		{"rfc1918 b", "http://10.0.0.7/", "ssrf guard"},
		{"rfc1918 c", "http://172.16.0.1/", "ssrf guard"},
		{"link local", "http://169.254.169.254/latest/meta-data/", "ssrf guard"},
		{"ipv6 loopback", "http://[::1]/", "ssrf guard"},
		{"local suffix", "http://foo.local/", "ssrf guard"},
		{"empty host", "http:///path", "url host is required"},
		{"bad scheme", "ftp://example.com/", "scheme must be http or https"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			out := runWeb(t, tool, map[string]any{"url": tt.url})
			if out["success"] == true {
				t.Fatalf("want refusal, got %v", out)
			}
			msg, _ := out["error"].(string)
			if !strings.Contains(msg, tt.want) {
				t.Errorf("error = %q, want contain %q", msg, tt.want)
			}
		})
	}
	// public hosts pass the guard (checked via unit predicate, no network)
	for _, h := range []string{"example.com", "8.8.8.8", "1.1.1.1"} {
		if err := checkPublicHost(h); err != nil {
			t.Errorf("checkPublicHost(%q) = %v, want nil", h, err)
		}
	}
}

func TestWebFetchInvalidInput(t *testing.T) {
	tool := newTestFetch(5*time.Second, 20000)
	out := runWeb(t, tool, map[string]any{})
	if out["success"] == true {
		t.Error("missing url must fail")
	}
	out = runWeb(t, tool, map[string]any{"url": "://bad url\x7f"})
	if out["success"] == true {
		t.Error("invalid url must fail")
	}
}

func TestWebFetchProxySeam(t *testing.T) {
	var proxied bool
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = true
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("via-proxy"))
	}))
	defer proxySrv.Close()

	tool := newTestFetch(5*time.Second, 20000)
	pu, err := url.Parse(proxySrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tool.proxy = func(*http.Request) (*url.URL, error) { return pu, nil }

	// target is unreachable directly but requests go through the proxy
	out := runWeb(t, tool, map[string]any{"url": "http://unreachable.invalid/."})
	if out["success"] != true {
		t.Fatalf("want proxy success, got %v", out)
	}
	if !proxied {
		t.Error("proxy function not used")
	}
	if out["output"] != "via-proxy" {
		t.Errorf("output = %q", out["output"])
	}
	// the default client must carry the env-based resolver (the documented
	// HTTP_PROXY/HTTPS_PROXY entry point; http.ProxyFromEnvironment caches
	// its env snapshot, so assert wiring rather than a live env value)
	def := NewWebFetchTool(5*time.Second, 20000)
	if def.clientOr().Transport.(*http.Transport).Proxy == nil {
		t.Error("default transport has no proxy resolver")
	}
}

func TestHTMLToText(t *testing.T) {
	got := htmlToText(`<html><script>x()</script><style>a{}</style><noscript>ns</noscript>` +
		`<body><b>bold</b><br>line2&nbsp;&nbsp;ok</body></html>`)
	if strings.Contains(got, "x()") || strings.Contains(got, "a{}") || strings.Contains(got, "ns") {
		t.Errorf("script/style/noscript leaked: %q", got)
	}
	if !strings.Contains(got, "bold") || !strings.Contains(got, "line2 ok") {
		t.Errorf("text lost: %q", got)
	}
	if got != strings.TrimSpace(got) {
		t.Errorf("not trimmed: %q", got)
	}
}

// ---- web_search ----

type fakeProvider struct {
	results  []SearchResult
	err      error
	gotQuery string
}

func (f *fakeProvider) Search(ctx context.Context, query string) ([]SearchResult, error) {
	f.gotQuery = query
	return f.results, f.err
}

// blockingProvider hangs until the context deadline, like a slow network.
type blockingProvider struct{}

func (blockingProvider) Search(ctx context.Context, query string) ([]SearchResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestWebSearchWithProvider(t *testing.T) {
	fp := &fakeProvider{results: []SearchResult{
		{Title: "r1", URL: "http://a", Snip: "s1"},
		{Title: "r2", URL: "http://b", Snip: "s2"},
		{Title: "r3", URL: "http://c", Snip: "s3"},
	}}
	tool := NewWebSearchTool(fp, 5*time.Second, 20000)
	out := runWeb(t, tool, map[string]any{"query": "go lang", "limit": 2})
	if out["success"] != true {
		t.Fatalf("want success, got %v", out)
	}
	if fp.gotQuery != "go lang" {
		t.Errorf("query = %q", fp.gotQuery)
	}
	results, _ := out["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v", out["results"])
	}
	first := results[0].(map[string]any)
	if first["title"] != "r1" || first["url"] != "http://a" {
		t.Errorf("first = %v", first)
	}
}

func TestWebSearchErrors(t *testing.T) {
	// provider failure surfaces a clear error
	fp := &fakeProvider{err: errors.New("quota exceeded")}
	tool := NewWebSearchTool(fp, 5*time.Second, 20000)
	out := runWeb(t, tool, map[string]any{"query": "x"})
	if out["success"] == true {
		t.Fatal("provider error must fail")
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "quota exceeded") {
		t.Errorf("error = %q", msg)
	}
	// missing query
	out = runWeb(t, tool, map[string]any{})
	if out["success"] == true {
		t.Error("missing query must fail")
	}
	// timeout classification: a provider that hangs until the deadline
	block := &blockingProvider{}
	tool = NewWebSearchTool(block, 10*time.Millisecond, 20000)
	out = runWeb(t, tool, map[string]any{"query": "x"})
	if out["timed_out"] != true {
		t.Errorf("timed_out = %v", out)
	}
}

func TestWebSearchDefaultProviderSeam(t *testing.T) {
	tool := NewWebSearchTool(nil, 5*time.Second, 20000)
	if _, ok := tool.provider.(*DuckDuckGoProvider); !ok {
		t.Errorf("nil provider must default to DuckDuckGoProvider, got %T", tool.provider)
	}
	if tool.Name() != "web_search" {
		t.Errorf("name = %s", tool.Name())
	}
	schema := tool.Schema()
	if schema["type"] != "object" {
		t.Errorf("schema = %v", schema)
	}
}

const ddgSample = `<html><body>
<div class="results">
<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F&amp;rut=x">The Go <b>Programming</b> Language</a>
<a class="result__snippet" href="x">Go is an open source <b>language</b>.</a>
<a class="result__a" href="https://example.org/page">Plain Link</a>
<a class="result__snippet" href="y">Snippet two &amp; more</a>
<a class="result__a" href="/l/?uddg=https%3A%2F%2Fpkg.go.dev%2Fstd">Std Library</a>
</div></body></html>`

func TestParseDDGResults(t *testing.T) {
	results := parseDDGResults(ddgSample)
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3: %+v", len(results), results)
	}
	if results[0].Title != "The Go Programming Language" {
		t.Errorf("title = %q", results[0].Title)
	}
	if results[0].URL != "https://go.dev/" {
		t.Errorf("url = %q", results[0].URL)
	}
	if results[0].Snip != "Go is an open source language." {
		t.Errorf("snip = %q", results[0].Snip)
	}
	if results[1].URL != "https://example.org/page" {
		t.Errorf("url = %q", results[1].URL)
	}
	if results[2].URL != "https://pkg.go.dev/std" {
		t.Errorf("url = %q", results[2].URL)
	}
}

func TestDuckDuckGoProviderHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("q") != "golang" {
			t.Errorf("q = %q", r.Form.Get("q"))
		}
		w.Write([]byte(ddgSample))
	}))
	defer srv.Close()

	p := NewDuckDuckGoProvider(5 * time.Second)
	p.endpoint = srv.URL
	results, err := p.Search(context.Background(), "golang")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %+v", results)
	}

	// non-200 becomes a clear error
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	defer errSrv.Close()
	p.endpoint = errSrv.URL
	if _, err := p.Search(context.Background(), "golang"); err == nil {
		t.Error("non-200 must error")
	}
}
