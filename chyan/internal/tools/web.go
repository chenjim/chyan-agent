// web.go: web_fetch (HTTP GET + HTML to text) and web_search (pluggable
// search provider) tools (core.Tool) built on net/http only.
package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"chyan.tech/chyan/internal/core"
)

const (
	webDefaultTimeout = 20 * time.Second
	webDefaultMaxBody = 2 << 20 // fetch body read cap before truncation
	webUserAgent      = "chyan-web/1.0"
)

// ---- web_fetch ----

// WebFetchTool implements core.Tool: fetch a URL over HTTP GET and return
// a text rendering of the body. Honors HTTP_PROXY/HTTPS_PROXY via
// http.Transport.Proxy (the local convention for egress).
type WebFetchTool struct {
	timeout time.Duration // whole-request timeout
	maxOut  int           // tool output cap in bytes
	client  *http.Client  // nil = built per call from the seams below

	// test seams: proxy resolves the egress proxy (default reads
	// HTTP_PROXY/HTTPS_PROXY via http.ProxyFromEnvironment); allowPrivate
	// skips the SSRF host guard so tests can target loopback httptest
	// servers.
	proxy        func(*http.Request) (*url.URL, error)
	allowPrivate bool
}

// NewWebFetchTool builds the web_fetch tool. timeout <= 0 falls back to
// 20s, maxOut <= 0 falls back to 20000 bytes.
func NewWebFetchTool(timeout time.Duration, maxOut int) *WebFetchTool {
	if timeout <= 0 {
		timeout = webDefaultTimeout
	}
	if maxOut <= 0 {
		maxOut = 20000
	}
	return &WebFetchTool{timeout: timeout, maxOut: maxOut}
}

// Name implements core.Tool.
func (t *WebFetchTool) Name() string { return "web_fetch" }

// Schema implements core.Tool.
func (t *WebFetchTool) Schema() map[string]any {
	return obj(map[string]any{
		"url":       map[string]any{"type": "string", "description": "http(s) URL to fetch"},
		"max_bytes": map[string]any{"type": "integer", "description": "max response bytes to read; default 2097152"},
	}, "url")
}

// Execute implements core.Tool.
func (t *WebFetchTool) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	return marshalResult(t.run(ctx, call.Input), t.maxOut)
}

func (t *WebFetchTool) run(ctx context.Context, in map[string]any) map[string]any {
	rawURL, ok := strArg(in, "url")
	if !ok || rawURL == "" {
		return fail("url is required")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fail("invalid url: " + err.Error())
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fail("url scheme must be http or https")
	}
	// SSRF boundary: obvious local/private destinations are refused so a
	// model cannot probe loopback, link-local or RFC1918 services through
	// this tool. Hostname/IP-literal level only (no DNS-rebind protection);
	// adjust here if local targets ever need to be reachable.
	if !t.allowPrivate {
		if err := checkPublicHost(u.Hostname()); err != nil {
			return fail(err.Error())
		}
	}

	maxBody := intArg(in, "max_bytes", webDefaultMaxBody)
	if maxBody <= 0 || maxBody > webDefaultMaxBody {
		maxBody = webDefaultMaxBody
	}

	rctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fail("build request: " + err.Error())
	}
	req.Header.Set("User-Agent", webUserAgent)

	resp, err := t.clientOr().Do(req)
	if err != nil {
		if rctx.Err() == context.DeadlineExceeded {
			return map[string]any{"success": false, "error": fmt.Sprintf("fetch timed out after %s", t.timeout), "timed_out": true}
		}
		return fail("fetch failed: " + err.Error())
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBody)+1))
	if err != nil {
		return fail("read body: " + err.Error())
	}
	overLimit := len(body) > maxBody
	if overLimit {
		body = body[:maxBody]
	}
	text := body
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "html") || looksLikeHTML(body) {
		text = []byte(htmlToText(string(body)))
	}
	okStatus := resp.StatusCode >= 200 && resp.StatusCode < 400
	res := map[string]any{
		"success":   okStatus,
		"status":    resp.StatusCode,
		"url":       u.String(),
		"format":    "text",
		"output":    string(text),
		"truncated": overLimit,
	}
	if !okStatus {
		res["error"] = fmt.Sprintf("http status %d", resp.StatusCode)
	}
	capField(res, "output", t.maxOut)
	return res
}

// clientOr returns the injected client or builds one on the proxy seam
// (default http.ProxyFromEnvironment honors HTTP_PROXY/HTTPS_PROXY/NO_PROXY).
func (t *WebFetchTool) clientOr() *http.Client {
	if t.client != nil {
		return t.client
	}
	proxy := t.proxy
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}
	return &http.Client{
		Timeout:   t.timeout,
		Transport: &http.Transport{Proxy: proxy},
	}
}

// hostLocalNames are hostnames refused without DNS resolution.
var hostLocalNames = map[string]bool{"localhost": true, "ip6-localhost": true}

// checkPublicHost refuses destinations that are obviously on this machine
// or the local network: loopback (127.0.0.0/8, ::1), link-local
// (169.254.0.0/16, fe80::/10), RFC1918 (10/8, 172.16/12, 192.168/16) and
// other private/unique-local ranges. Public internet hosts pass.
func checkPublicHost(host string) error {
	if host == "" {
		return errors.New("url host is required")
	}
	h := strings.ToLower(strings.Trim(host, "[]"))
	if hostLocalNames[h] {
		return errors.New("fetch to local address refused (ssrf guard)")
	}
	ip := net.ParseIP(h)
	if ip == nil {
		// hostname: refuse obvious local suffixes; DNS names resolving to
		// private IPs are not checked (see SSRF boundary note above).
		if strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") {
			return errors.New("fetch to local address refused (ssrf guard)")
		}
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return errors.New("fetch to local address refused (ssrf guard)")
	}
	return nil
}

var (
	scriptRe = regexp.MustCompile(`(?is)<(script|style|noscript|svg)[^>]*>.*?</(script|style|noscript|svg)>`)
	tagRe    = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRe  = regexp.MustCompile(`[ \t\r\f\v]+`)
)

// looksLikeHTML does a cheap sniff when Content-Type is missing/wrong.
func looksLikeHTML(b []byte) bool {
	head := strings.ToLower(string(b[:min(len(b), 512)]))
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

// htmlToText strips script/style blocks and tags, collapses whitespace
// and decodes the handful of common entities. Deliberately regex-based:
// no HTML parser dependency (go.mod stays std-only additions).
func htmlToText(s string) string {
	s = scriptRe.ReplaceAllString(s, " ")
	s = tagRe.ReplaceAllString(s, " ")
	s = strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&apos;", "'",
	).Replace(s)
	s = spaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// ---- web_search ----

// SearchResult is one search hit.
type SearchResult struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Snip  string `json:"snippet"`
}

// SearchProvider abstracts a search source; swap implementations via
// NewWebSearchTool (the seam for real search backends later).
type SearchProvider interface {
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

// WebSearchTool implements core.Tool: query a pluggable SearchProvider
// and return ranked results.
type WebSearchTool struct {
	provider   SearchProvider // nil = DuckDuckGoProvider
	timeout    time.Duration
	maxOut     int
	maxResults int
}

// NewWebSearchTool builds the web_search tool with the default
// DuckDuckGo provider; nil provider falls back to it.
func NewWebSearchTool(provider SearchProvider, timeout time.Duration, maxOut int) *WebSearchTool {
	if provider == nil {
		provider = NewDuckDuckGoProvider(timeout)
	}
	if timeout <= 0 {
		timeout = webDefaultTimeout
	}
	if maxOut <= 0 {
		maxOut = 20000
	}
	return &WebSearchTool{provider: provider, timeout: timeout, maxOut: maxOut, maxResults: 8}
}

// Name implements core.Tool.
func (t *WebSearchTool) Name() string { return "web_search" }

// Schema implements core.Tool.
func (t *WebSearchTool) Schema() map[string]any {
	return obj(map[string]any{
		"query": map[string]any{"type": "string", "description": "search keywords"},
		"limit": map[string]any{"type": "integer", "description": "max results; default 8"},
	}, "query")
}

// Execute implements core.Tool.
func (t *WebSearchTool) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	return marshalResult(t.run(ctx, call.Input), t.maxOut)
}

func (t *WebSearchTool) run(ctx context.Context, in map[string]any) map[string]any {
	query, ok := strArg(in, "query")
	if !ok || strings.TrimSpace(query) == "" {
		return fail("query is required")
	}
	limit := intArg(in, "limit", t.maxResults)
	if limit <= 0 {
		limit = t.maxResults
	}

	rctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	results, err := t.provider.Search(rctx, query)
	if err != nil {
		if rctx.Err() == context.DeadlineExceeded {
			return map[string]any{"success": false, "error": fmt.Sprintf("search timed out after %s", t.timeout), "timed_out": true}
		}
		return fail("search failed: " + err.Error())
	}
	if len(results) > limit {
		results = results[:limit]
	}
	anyList := make([]any, len(results))
	for i, r := range results {
		anyList[i] = map[string]any{"title": r.Title, "url": r.URL, "snippet": r.Snip}
	}
	return map[string]any{"success": true, "query": query, "results": anyList}
}

// ---- DuckDuckGo provider ----

// DuckDuckGoProvider scrapes the no-JS html endpoint
// (html.duckduckgo.com/html/?q=...) which needs no API key.
type DuckDuckGoProvider struct {
	client   *http.Client
	endpoint string // empty = ddgEndpoint; overridable in tests
}

// NewDuckDuckGoProvider builds the default provider; timeout <= 0 falls
// back to 20s.
func NewDuckDuckGoProvider(timeout time.Duration) *DuckDuckGoProvider {
	if timeout <= 0 {
		timeout = webDefaultTimeout
	}
	return &DuckDuckGoProvider{client: &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
	}}
}

const ddgEndpoint = "https://html.duckduckgo.com/html/"

// Search implements SearchProvider.
func (p *DuckDuckGoProvider) Search(ctx context.Context, query string) ([]SearchResult, error) {
	endpoint := p.endpoint
	if endpoint == "" {
		endpoint = ddgEndpoint
	}
	form := url.Values{"q": {query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search endpoint status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, webDefaultMaxBody))
	if err != nil {
		return nil, err
	}
	return parseDDGResults(string(body)), nil
}

var (
	ddgResultRe = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__a[^"]*"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnipRe   = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>`)
	ddgStripTag = regexp.MustCompile(`(?s)<[^>]*>`)
)

// parseDDGResults extracts title/url/snippet triples from the html
// endpoint response. URLs are decoded from the /l/?uddg= redirect form.
func parseDDGResults(html string) []SearchResult {
	var out []SearchResult
	for _, m := range ddgResultRe.FindAllStringSubmatch(html, -1) {
		href := decodeDDGURL(m[1])
		title := cleanTagText(m[2])
		if href == "" || title == "" {
			continue
		}
		out = append(out, SearchResult{Title: title, URL: href})
	}
	snips := ddgSnipRe.FindAllStringSubmatch(html, -1)
	for i := range out {
		if i < len(snips) {
			out[i].Snip = cleanTagText(snips[i][1])
		}
	}
	return out
}

// decodeDDGURL unwraps the redirect href form /l/?uddg=<encoded>&... into
// the real target URL; absolute http(s) hrefs pass through unchanged.
func decodeDDGURL(href string) string {
	href = strings.TrimSpace(htmlToTextEntities(href))
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	if u, err := url.Parse(href); err == nil {
		if target := u.Query().Get("uddg"); target != "" {
			return target
		}
	}
	return href
}

// htmlToTextEntities decodes the few entities that can appear inside
// attribute values (hrefs) without touching the rest.
func htmlToTextEntities(s string) string {
	return strings.NewReplacer("&amp;", "&", "&#38;", "&", "&quot;", `"`, "&#39;", "'").Replace(s)
}

// cleanTagText strips nested tags and collapses whitespace.
func cleanTagText(s string) string {
	s = ddgStripTag.ReplaceAllString(s, " ")
	s = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(s)
	// tag-to-space joins leave "word ." behind; drop that space
	s = punctSpaceRe.ReplaceAllString(s, "$1")
	return strings.Join(strings.Fields(s), " ")
}

var punctSpaceRe = regexp.MustCompile(` ([.,;:!?)])`)
