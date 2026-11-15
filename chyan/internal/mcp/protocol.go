// Package mcp implements the MCP client (architecture 4.8, research D17:
// client only): remote HTTP and local stdio transports, tool catalog
// fetch, and core.Tool adapters registering MCP tools as <server>_<tool>.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// JSON-RPC 2.0 wire types for the minimal MCP surface (tools/list, tools/call).
type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("mcp: rpc error %d: %s", e.Code, e.Message)
}

// maxMessageBytes caps a single JSON-RPC message on either transport.
const maxMessageBytes = 8 << 20

// rpcClient issues sequential JSON-RPC calls and matches responses by id.
type rpcClient struct {
	nextID atomic.Int64

	mu    sync.Mutex // serializes calls (writes + response pairing)
	call  func(ctx context.Context, req request) (json.RawMessage, *rpcError, error)
	close func() error
}

// do performs one request/response exchange and decodes the result.
func (c *rpcClient) do(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, rpcErr, err := c.call(ctx, request{
		JSONRPC: "2.0",
		ID:      c.nextID.Add(1),
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return err
	}
	if rpcErr != nil {
		return rpcErr
	}
	if result != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, result); err != nil {
			return fmt.Errorf("mcp: decode %s result: %w", method, err)
		}
	}
	return nil
}

// shutdown closes the underlying transport (kills the stdio child).
func (c *rpcClient) shutdown() error {
	if c.close != nil {
		return c.close()
	}
	return nil
}

// ---- remote transport: JSON-RPC over single HTTP POST ----

// remoteTransport posts one JSON-RPC message per HTTP request with the
// configured headers (credentials resolved by the caller).
type remoteTransport struct {
	url     string
	headers map[string]string
	client  *http.Client
}

func (t *remoteTransport) call(ctx context.Context, req request) (json.RawMessage, *rpcError, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	for k, v := range t.headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: http %s: %w", t.url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMessageBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("mcp: http %s: status %d", t.url, resp.StatusCode)
	}
	var rpcResp response
	if err := json.Unmarshal(data, &rpcResp); err != nil {
		return nil, nil, fmt.Errorf("mcp: decode response: %w", err)
	}
	if rpcResp.ID != req.ID {
		return nil, nil, fmt.Errorf("mcp: response id %d != request id %d", rpcResp.ID, req.ID)
	}
	return rpcResp.Result, rpcResp.Error, nil
}

// ---- local transport: JSON-RPC over newline-delimited stdio ----

// stdioTransport runs a child process speaking line-delimited JSON-RPC.
// One reader goroutine owns stdout and dispatches responses to the pending
// request by id: concurrent calls therefore never steal each other's reply,
// and no goroutine outlives a canceled call.
type stdioTransport struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *lineReader
	exited   chan struct{} // closed once the child is reaped
	readDone chan struct{} // closed after stdout is drained and waiters are notified
	exitMu   sync.Mutex    // guards exitErr
	exitErr  error
	done     atomic.Bool

	pendingMu sync.Mutex
	pending   map[int64]chan rpcResult
}

// rpcResult is one matched response handed to a waiting call.
type rpcResult struct {
	res    json.RawMessage
	rpcErr *rpcError
	err    error
}

func startStdio(ctx context.Context, argv []string) (*stdioTransport, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("mcp: empty command")
	}
	cctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mcp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mcp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("mcp: start %s: %w", argv[0], err)
	}
	t := &stdioTransport{
		cmd: cmd, stdin: stdin, stdout: newLineReader(stdout),
		exited:   make(chan struct{}),
		readDone: make(chan struct{}),
		pending:  map[int64]chan rpcResult{},
	}
	go t.readLoop()
	go func() {
		err := cmd.Wait()
		t.exitMu.Lock()
		t.exitErr = err
		t.exitMu.Unlock()
		close(t.exited)
		t.done.Store(true)
		cancel() // release the context once the child exits
	}()
	return t, nil
}

// readLoop is the only stdout consumer: it matches responses by id and fails
// every waiter when the pipe ends.
func (t *stdioTransport) readLoop() {
	defer close(t.readDone)
	for {
		raw, err := t.stdout.readLine()
		if err != nil {
			t.failPending(fmt.Errorf("mcp: read stdout: %w", err))
			return
		}
		var rpcResp response
		if err := json.Unmarshal(raw, &rpcResp); err != nil {
			continue // log noise or notification, not a response
		}
		if rpcResp.ID == 0 {
			continue // server notification
		}
		t.pendingMu.Lock()
		ch := t.pending[rpcResp.ID]
		delete(t.pending, rpcResp.ID)
		t.pendingMu.Unlock()
		if ch != nil {
			ch <- rpcResult{res: rpcResp.Result, rpcErr: rpcResp.Error}
		}
	}
}

// failPending delivers err to every waiting call.
func (t *stdioTransport) failPending(err error) {
	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()
	for id, ch := range t.pending {
		delete(t.pending, id)
		ch <- rpcResult{err: err}
	}
}

// call writes one request line and waits for the matching response, the child
// exiting, or ctx ending.
func (t *stdioTransport) call(ctx context.Context, req request) (json.RawMessage, *rpcError, error) {
	if t.done.Load() {
		return nil, nil, fmt.Errorf("mcp: transport closed")
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: encode request: %w", err)
	}
	line = append(line, '\n')

	ch := make(chan rpcResult, 1)
	t.pendingMu.Lock()
	if t.done.Load() {
		t.pendingMu.Unlock()
		return nil, nil, fmt.Errorf("mcp: transport closed")
	}
	t.pending[req.ID] = ch
	t.pendingMu.Unlock()
	defer func() {
		t.pendingMu.Lock()
		delete(t.pending, req.ID)
		t.pendingMu.Unlock()
	}()

	// A pipe write blocks while the child stops reading: run it aside so a
	// timeout can break the transport instead of hanging the agent loop.
	werr := make(chan error, 1)
	go func() { _, err := t.stdin.Write(line); werr <- err }()
	select {
	case err := <-werr:
		if err != nil {
			return nil, nil, fmt.Errorf("mcp: write stdin: %w", err)
		}
	case <-t.exited:
		// The child may have written a valid reply immediately before exiting.
		// Drain stdout before deciding whether this was a failed exchange.
		<-t.readDone
		select {
		case r := <-ch:
			return r.res, r.rpcErr, r.err
		default:
		}
		return nil, nil, t.exitError()
	case <-ctx.Done():
		t.kill() // unblocks the orphaned writer
		return nil, nil, fmt.Errorf("mcp: %w", ctx.Err())
	}

	select {
	case r := <-ch:
		return r.res, r.rpcErr, r.err
	case <-t.exited:
		// exited can win the select before readLoop delivers an already-read
		// response. Waiting for readDone makes response-vs-EOF ordering explicit.
		<-t.readDone
		select {
		case r := <-ch:
			return r.res, r.rpcErr, r.err
		default:
		}
		return nil, nil, t.exitError()
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("mcp: %w", ctx.Err())
	}
}

// exitError describes the reaped child.
func (t *stdioTransport) exitError() error {
	t.exitMu.Lock()
	err := t.exitErr
	t.exitMu.Unlock()
	if err != nil {
		return fmt.Errorf("mcp: process exited: %w", err)
	}
	return fmt.Errorf("mcp: process exited")
}

// kill tears the transport down; it is idempotent.
func (t *stdioTransport) kill() {
	if t.done.Swap(true) {
		return
	}
	_ = t.stdin.Close()
	_ = t.cmd.Process.Kill()
	<-t.exited
}

func (t *stdioTransport) shutdown() error {
	t.kill()
	return nil
}

// lineReader reads '\n'-terminated lines with a cap (bufio.Scanner's 64KB
// default is too small for tool catalogs).
type lineReader struct {
	mu sync.Mutex
	r  io.Reader
	b  []byte
}

func newLineReader(r io.Reader) *lineReader { return &lineReader{r: r} }

// readLine returns one '\n'-terminated line without the newline. Lines
// longer than maxMessageBytes are an error (defensive cap).
func (s *lineReader) readLine() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if i := bytes.IndexByte(s.b, '\n'); i >= 0 {
			if i > maxMessageBytes {
				return nil, fmt.Errorf("mcp: line exceeds %d bytes", maxMessageBytes)
			}
			line := append([]byte(nil), s.b[:i]...)
			s.b = s.b[i+1:]
			return line, nil
		}
		if len(s.b) > maxMessageBytes {
			return nil, fmt.Errorf("mcp: line exceeds %d bytes", maxMessageBytes)
		}
		buf := make([]byte, 32<<10)
		n, err := s.r.Read(buf)
		if n > 0 {
			s.b = append(s.b, buf[:n]...)
		}
		if err != nil {
			if len(s.b) > 0 {
				line := s.b
				s.b = nil
				return line, nil // trailing line without newline
			}
			return nil, err
		}
	}
}

// ---- minimal MCP method surface ----

// mcpTool is one entry of the tools/list catalog.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// toolOutput is the normalized tools/call result payload (MCP content
// blocks kept raw; rendered by renderOutput).
type toolOutput struct {
	Content    json.RawMessage `json:"content"`
	IsError    bool            `json:"isError"`
	Structured json.RawMessage `json:"structuredContent,omitempty"`
}

// listTools fetches the server tool catalog (tools/list).
func (c *rpcClient) listTools(ctx context.Context) ([]mcpTool, error) {
	var out struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := c.do(ctx, "tools/list", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// callTool invokes one server tool (tools/call). input may be nil.
func (c *rpcClient) callTool(ctx context.Context, name string, input map[string]any) (toolOutput, error) {
	if input == nil {
		input = map[string]any{}
	}
	var out toolOutput
	if err := c.do(ctx, "tools/call", map[string]any{"name": name, "arguments": input}, &out); err != nil {
		return toolOutput{}, err
	}
	return out, nil
}

// toolTimeout resolves an ms-based timeout, applying the default when unset.
func toolTimeout(ms, def int) time.Duration {
	if ms <= 0 {
		ms = def
	}
	return time.Duration(ms) * time.Millisecond
}
