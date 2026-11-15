package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSecrets resolves only known credential names.
type fakeSecrets struct {
	values map[string]string
	calls  int
}

func (f *fakeSecrets) ResolveSecrets(raw string) (string, error) {
	f.calls++
	out := raw
	for name, val := range f.values {
		out = strings.ReplaceAll(out, "secret:"+name, val)
	}
	if strings.Contains(out, "secret:") {
		return "", errors.New("secret not found")
	}
	return out, nil
}

func TestResolveHeaders(t *testing.T) {
	sec := &fakeSecrets{values: map[string]string{"tok": "s3cret"}}

	got, err := resolveHeaders(map[string]string{"Authorization": "secret:tok", "X-Plain": "v"}, sec)
	if err != nil {
		t.Fatalf("resolveHeaders: %v", err)
	}
	if got["Authorization"] != "s3cret" || got["X-Plain"] != "v" {
		t.Errorf("headers = %+v", got)
	}

	// Missing secret errors without leaking the raw value.
	_, err = resolveHeaders(map[string]string{"Authorization": "secret:nope"}, sec)
	if err == nil || strings.Contains(err.Error(), "secret:nope") {
		t.Errorf("missing secret error = %v", err)
	}

	// Secret reference without a store is an error.
	if _, err := resolveHeaders(map[string]string{"A": "secret:tok"}, nil); err == nil {
		t.Error("nil store should error")
	}

	// No headers / plain-only short-circuit.
	if got, err := resolveHeaders(nil, nil); err != nil || got != nil {
		t.Errorf("nil headers = %+v, %v", got, err)
	}
	if got, err := resolveHeaders(map[string]string{"A": "b"}, nil); err != nil || got["A"] != "b" {
		t.Errorf("plain headers = %+v, %v", got, err)
	}
}

func TestServerEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  ServerConfig
		want bool
	}{
		{"remote ok", ServerConfig{Type: "remote", URL: "http://x"}, true},
		{"remote no url", ServerConfig{Type: "remote"}, false},
		{"local ok", ServerConfig{Type: "local", Command: []string{"x"}}, true},
		{"local no cmd", ServerConfig{Type: "local"}, false},
		{"disabled", ServerConfig{Type: "remote", URL: "http://x", Disabled: true}, false},
		{"unknown type", ServerConfig{Type: "tcp"}, false},
	}
	for _, tt := range tests {
		if got := newServer(tt.cfg, nil).Enabled(); got != tt.want {
			t.Errorf("%s: Enabled = %v", tt.name, got)
		}
	}
}

// dialRecorder injects a fake connection and counts dial attempts.
type dialRecorder struct {
	calls atomic.Int32
	err   error
	rpc   *rpcClient
}

func (d *dialRecorder) dial(ctx context.Context, cfg ServerConfig, s Secrets) (*rpcClient, error) {
	d.calls.Add(1)
	if d.err != nil {
		return nil, d.err
	}
	return d.rpc, nil
}

func stubRPC(result string) *rpcClient {
	return &rpcClient{call: func(ctx context.Context, req request) (json.RawMessage, *rpcError, error) {
		return []byte(result), nil, nil
	}}
}

func TestServerBackoffAndRecovery(t *testing.T) {
	rec := &dialRecorder{err: errors.New("dial failed")}
	s := newServer(ServerConfig{Name: "s", Type: "remote", URL: "http://x"}, nil)
	s.dial = rec.dial

	if _, err := s.listTools(context.Background()); err == nil {
		t.Fatal("first dial should fail")
	}
	if got := rec.calls.Load(); got != 1 {
		t.Fatalf("dial calls = %d", got)
	}
	// Second call inside the backoff window fails fast without dialing.
	if _, err := s.listTools(context.Background()); err == nil || !strings.Contains(err.Error(), "retry in") {
		t.Fatalf("backoff error = %v", err)
	}
	if got := rec.calls.Load(); got != 1 {
		t.Fatalf("dial calls after backoff = %d", got)
	}

	// After the window the dial is retried; success resets the backoff.
	s.mu.Lock()
	s.lastDial = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	rec.err = nil
	rec.rpc = stubRPC(`{"tools":[]}`)
	if _, err := s.listTools(context.Background()); err != nil {
		t.Fatalf("recovered listTools: %v", err)
	}
	if !s.Available() {
		t.Error("server should be available")
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if s.Available() {
		t.Error("server should be closed")
	}
}

func TestServerCallToolDropsBrokenConn(t *testing.T) {
	rec := &dialRecorder{}
	rec.rpc = &rpcClient{call: func(ctx context.Context, req request) (json.RawMessage, *rpcError, error) {
		return nil, nil, errors.New("conn reset")
	}}
	s := newServer(ServerConfig{Name: "s", Type: "local", Command: []string{"x"}}, nil)
	s.dial = rec.dial

	if _, err := s.callTool(context.Background(), "t", nil); err == nil {
		t.Fatal("broken call should fail")
	}
	if s.Available() {
		t.Error("broken connection should be dropped")
	}
	// Backoff was reset on successful dial: the next call redials immediately.
	rec.rpc = stubRPC(`{"content":[]}`)
	if _, err := s.callTool(context.Background(), "t", nil); err != nil {
		t.Fatalf("redial call: %v", err)
	}
}

func TestServerTimeouts(t *testing.T) {
	cfg := ServerConfig{Name: "s", Type: "remote", URL: "http://x", CatalogTimeout: 20, ExecutionTimeout: 30}
	if got := catalogTimeout(cfg); got != 20*time.Millisecond {
		t.Errorf("catalogTimeout = %v", got)
	}
	if got := executionTimeout(cfg); got != 30*time.Millisecond {
		t.Errorf("executionTimeout = %v", got)
	}
	cfg = ServerConfig{}
	if got := catalogTimeout(cfg); got != defaultCatalogTimeout*time.Millisecond {
		t.Errorf("default catalogTimeout = %v", got)
	}
	if got := executionTimeout(cfg); got != defaultExecutionTimeout*time.Millisecond {
		t.Errorf("default executionTimeout = %v", got)
	}
}

func TestServerDialUnknownType(t *testing.T) {
	s := newServer(ServerConfig{Name: "s", Type: "weird"}, nil)
	if _, err := s.dialNow(context.Background()); err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Errorf("dialNow = %v", err)
	}
}

func TestServerDialRemoteSecretError(t *testing.T) {
	s := newServer(ServerConfig{
		Name: "s", Type: "remote", URL: "http://x",
		Headers: map[string]string{"Authorization": "secret:missing"},
	}, &fakeSecrets{values: map[string]string{}})
	if _, err := s.dialNow(context.Background()); err == nil || !strings.Contains(err.Error(), "header Authorization") {
		t.Errorf("dialNow = %v", err)
	}
}

func TestServerDialLocalBadCommand(t *testing.T) {
	s := newServer(ServerConfig{Name: "s", Type: "local", Command: []string{"/no/such/bin"}}, nil)
	if _, err := s.dialNow(context.Background()); err == nil {
		t.Error("bad command should fail")
	}
}
