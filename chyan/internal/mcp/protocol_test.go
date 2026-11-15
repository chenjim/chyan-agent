package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fake remote server (httptest) ----

// fakeRemote answers tools/list and tools/call JSON-RPC over HTTP POST.
type fakeRemote struct {
	tools     []mcpTool
	callText  string
	callError bool
	rpcError  bool
	rawBody   string // when non-empty, sent verbatim as the response body
	lastAuth  string
	lastReq   request
}

func (f *fakeRemote) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		f.lastReq = req
		if f.rawBody != "" {
			fmt.Fprint(w, strings.ReplaceAll(f.rawBody, "{ID}", fmt.Sprintf("%d", req.ID)))
			return
		}
		if f.rpcError {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"no method"}}`, req.ID)
			return
		}
		var result string
		switch req.Method {
		case "tools/list":
			b, _ := json.Marshal(map[string]any{"tools": f.tools})
			result = string(b)
		case "tools/call":
			b, _ := json.Marshal(map[string]any{
				"content": []map[string]any{{"type": "text", "text": f.callText}},
				"isError": f.callError,
			})
			result = string(b)
		default:
			t.Errorf("unexpected method %q", req.Method)
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, result)
	})
}

func startRemote(t *testing.T, f *fakeRemote) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return srv
}

// ---- remote transport ----

func TestRemoteListAndCall(t *testing.T) {
	f := &fakeRemote{tools: []mcpTool{{Name: "search", Description: "d"}}, callText: "hi"}
	srv := startRemote(t, f)
	rpc := &rpcClient{call: (&remoteTransport{url: srv.URL, client: &http.Client{}}).call}

	tools, err := rpc.listTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "search" {
		t.Fatalf("listTools = %+v, %v", tools, err)
	}
	out, err := rpc.callTool(context.Background(), "search", map[string]any{"q": "x"})
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if got := renderOutput(out); got != "hi" {
		t.Errorf("renderOutput = %q", got)
	}
	if f.lastReq.Method != "tools/call" {
		t.Errorf("last method = %q", f.lastReq.Method)
	}
}

func TestRemoteCallNilInput(t *testing.T) {
	f := &fakeRemote{callText: "ok"}
	srv := startRemote(t, f)
	rpc := &rpcClient{call: (&remoteTransport{url: srv.URL, client: &http.Client{}}).call}
	if _, err := rpc.callTool(context.Background(), "t", nil); err != nil {
		t.Fatalf("callTool: %v", err)
	}
	args, _ := json.Marshal(f.lastReq.Params.(map[string]any)["arguments"])
	if string(args) != "{}" {
		t.Errorf("arguments = %s", args)
	}
}

func TestRemoteErrors(t *testing.T) {
	f := &fakeRemote{}
	srv := startRemote(t, f)
	transport := &remoteTransport{url: srv.URL, client: &http.Client{}}
	rpc := &rpcClient{call: transport.call}

	// JSON-RPC error object surfaces as *rpcError.
	f.rpcError = true
	if _, err := rpc.listTools(context.Background()); err == nil || !strings.Contains(err.Error(), "-32601") {
		t.Errorf("rpc error = %v", err)
	}
	f.rpcError = false

	// HTTP error status.
	transport.url = srv.URL + "/missing"
	if _, err := rpc.listTools(context.Background()); err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Errorf("status error = %v", err)
	}
	transport.url = srv.URL

	// Malformed body.
	f.rawBody = "not json"
	if _, err := rpc.listTools(context.Background()); err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Errorf("bad body error = %v", err)
	}

	// Response id mismatch.
	f.rawBody = `{"jsonrpc":"2.0","id":99,"result":{}}`
	if _, err := rpc.listTools(context.Background()); err == nil || !strings.Contains(err.Error(), "response id") {
		t.Errorf("id mismatch error = %v", err)
	}
	f.rawBody = ""

	// Un-decodable result payload.
	f.rawBody = `{"jsonrpc":"2.0","id":{ID},"result":"not-an-object"}`
	if _, err := rpc.listTools(context.Background()); err == nil || !strings.Contains(err.Error(), "decode tools/list") {
		t.Errorf("bad result error = %v", err)
	}
}

// ---- stdio transport ----

// TestHelperStdioServer is a fake MCP stdio server run as a child of the
// test binary (no network, no external dependencies). CHYAN_MCP_HELPER=1
// enables it; CHYAN_MCP_HELPER_SLEEP delays each response and
// CHYAN_MCP_HELPER_EXIT_AFTER_RESPONSE exits immediately after the first reply.
func TestHelperStdioServer(t *testing.T) {
	if os.Getenv("CHYAN_MCP_HELPER") != "1" {
		t.Skip("helper process")
	}
	sleep := time.Duration(0)
	if v := os.Getenv("CHYAN_MCP_HELPER_SLEEP"); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			sleep = d
		}
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var req request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		if sleep > 0 {
			time.Sleep(sleep)
		}
		var result string
		switch req.Method {
		case "tools/list":
			result = `{"tools":[{"name":"echo","description":"echo back","inputSchema":{"type":"object"}}]}`
		case "tools/call":
			result = `{"content":[{"type":"text","text":"pong"}]}`
		default:
			result = `{}`
		}
		fmt.Printf(`{"jsonrpc":"2.0","id":%d,"result":%s}`+"\n", req.ID, result)
		if os.Getenv("CHYAN_MCP_HELPER_EXIT_AFTER_RESPONSE") == "1" {
			os.Exit(0)
		}
	}
	os.Exit(0)
}

func helperCommand() []string {
	return []string{os.Args[0], "-test.run=TestHelperStdioServer"}
}

func TestStdioHelperRoundTrip(t *testing.T) {
	t.Setenv("CHYAN_MCP_HELPER", "1")
	t.Setenv("CHYAN_MCP_HELPER_SLEEP", "")
	rpc, err := startStdio(context.Background(), helperCommand())
	if err != nil {
		t.Fatalf("startStdio: %v", err)
	}
	t.Cleanup(func() { rpc.shutdown() })
	client := &rpcClient{call: rpc.call, close: rpc.shutdown}

	tools, err := client.listTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("listTools = %+v, %v", tools, err)
	}
	out, err := client.callTool(context.Background(), "echo", nil)
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if got := renderOutput(out); got != "pong" {
		t.Errorf("renderOutput = %q", got)
	}
}

// TestStdioConcurrentCallsKeepOwnResponses drives the transport directly:
// concurrent requests must each receive the response carrying their own id.
func TestStdioConcurrentCallsKeepOwnResponses(t *testing.T) {
	t.Setenv("CHYAN_MCP_HELPER", "1")
	t.Setenv("CHYAN_MCP_HELPER_SLEEP", "")
	rpc, err := startStdio(context.Background(), helperCommand())
	if err != nil {
		t.Fatalf("startStdio: %v", err)
	}
	defer rpc.shutdown()

	const n = 5
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			raw, rpcErr, err := rpc.call(ctx, request{JSONRPC: "2.0", ID: id, Method: "tools/call"})
			if err != nil || rpcErr != nil {
				errs <- fmt.Errorf("call %d: %v %v", id, err, rpcErr)
				return
			}
			if !strings.Contains(string(raw), "pong") {
				errs <- fmt.Errorf("call %d got %s", id, raw)
			}
		}(int64(1000 + i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestStdioResponseImmediatelyBeforeExit(t *testing.T) {
	t.Setenv("CHYAN_MCP_HELPER", "1")
	t.Setenv("CHYAN_MCP_HELPER_SLEEP", "")
	t.Setenv("CHYAN_MCP_HELPER_EXIT_AFTER_RESPONSE", "1")
	for i := 0; i < 20; i++ {
		rpc, err := startStdio(context.Background(), helperCommand())
		if err != nil {
			t.Fatalf("iteration %d: startStdio: %v", i, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		raw, rpcErr, err := rpc.call(ctx, request{JSONRPC: "2.0", ID: int64(i + 1), Method: "tools/call"})
		cancel()
		_ = rpc.shutdown()
		if err != nil || rpcErr != nil {
			t.Fatalf("iteration %d: lost final response: %v %v", i, err, rpcErr)
		}
		if !strings.Contains(string(raw), "pong") {
			t.Fatalf("iteration %d: response = %s", i, raw)
		}
	}
}

func TestStdioWithCat(t *testing.T) {
	// cat echoes each request line back: id pairing must still match.
	rpc, err := startStdio(context.Background(), []string{"cat"})
	if err != nil {
		t.Fatalf("startStdio: %v", err)
	}
	client := &rpcClient{call: rpc.call, close: rpc.shutdown}
	if _, err := client.listTools(context.Background()); err != nil {
		t.Fatalf("listTools via cat: %v", err)
	}
	_ = rpc.shutdown()
	if _, err := client.listTools(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("after shutdown = %v", err)
	}
}

func TestStdioErrors(t *testing.T) {
	if _, err := startStdio(context.Background(), nil); err == nil {
		t.Error("empty command should fail")
	}
	if _, err := startStdio(context.Background(), []string{"/nonexistent-cmd-xyz"}); err == nil {
		t.Error("missing binary should fail")
	}

	// Child exits without responding.
	rpc, err := startStdio(context.Background(), []string{"true"})
	if err != nil {
		t.Fatalf("startStdio: %v", err)
	}
	client := &rpcClient{call: rpc.call, close: rpc.shutdown}
	if _, err := client.listTools(context.Background()); err == nil {
		t.Error("exited child should fail the call")
	}

	// Context cancellation on a silent child.
	t.Setenv("CHYAN_MCP_HELPER", "1")
	t.Setenv("CHYAN_MCP_HELPER_SLEEP", "10s")
	rpc2, err := startStdio(context.Background(), helperCommand())
	if err != nil {
		t.Fatalf("startStdio: %v", err)
	}
	defer rpc2.shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	client2 := &rpcClient{call: rpc2.call}
	if _, err := client2.listTools(ctx); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("timeout error = %v", err)
	}
}

// ---- lineReader ----

func TestLineReader(t *testing.T) {
	r := newLineReader(strings.NewReader("a\nbb\nno-newline"))
	for i, want := range []string{"a", "bb", "no-newline"} {
		got, err := r.readLine()
		if err != nil || string(got) != want {
			t.Fatalf("line %d = %q, %v", i, got, err)
		}
	}
	if _, err := r.readLine(); err == nil {
		t.Error("EOF expected")
	}

	// Over-long line is rejected.
	big := strings.Repeat("x", maxMessageBytes+2) + "\n"
	if _, err := newLineReader(strings.NewReader(big)).readLine(); err == nil {
		t.Error("oversized line should fail")
	}
}

// ---- helpers ----

func TestToolTimeout(t *testing.T) {
	if got := toolTimeout(0, 30000); got != 30*time.Second {
		t.Errorf("default = %v", got)
	}
	if got := toolTimeout(50, 30000); got != 50*time.Millisecond {
		t.Errorf("explicit = %v", got)
	}
	if got := toolTimeout(-1, 30000); got != 30*time.Second {
		t.Errorf("negative = %v", got)
	}
}

func TestRPCClientShutdownNilClose(t *testing.T) {
	c := &rpcClient{}
	if err := c.shutdown(); err != nil {
		t.Errorf("shutdown = %v", err)
	}
}
