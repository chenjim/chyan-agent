package mcp

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Default timeouts (ms, aligned with the opencode config shape) and
// reconnection backoff.
const (
	defaultCatalogTimeout   = 30000
	defaultExecutionTimeout = 30000
	initialBackoff          = 1 * time.Second
	maxBackoff              = 30 * time.Second
)

// ServerConfig is one [mcp.servers.<name>] entry (architecture 4.8). The
// config package owns TOML parsing; this package receives parsed structs.
type ServerConfig struct {
	Name             string
	Type             string            // remote|local
	URL              string            // remote endpoint
	Command          []string          // local argv
	Headers          map[string]string // remote headers; values may be "secret:<name>"
	CatalogTimeout   int               // ms; 0 = default
	ExecutionTimeout int               // ms; 0 = default
	Disabled         bool
}

// Secrets resolves "secret:<name>" credential references (chyan/internal/secret).
type Secrets interface {
	ResolveSecrets(raw string) (string, error)
}

// Server is one MCP server connection. A server whose initial connect or
// catalog fetch failed stays degraded: its tools are unavailable but other
// servers keep working; the next call retries the connect with backoff.
type Server struct {
	cfg     ServerConfig
	secrets Secrets
	// dial, when set, overrides transport construction (tests only).
	dial func(ctx context.Context, cfg ServerConfig, secrets Secrets) (*rpcClient, error)

	mu      sync.Mutex
	rpc     *rpcClient
	backoff time.Duration
	// lastDial is the last (successful or failed) connect attempt time;
	// before it plus the current backoff, calls fail fast without dialing.
	lastDial time.Time
	// unavailable tracks degraded state for observability/logging.
	unavailable bool
}

func newServer(cfg ServerConfig, secrets Secrets) *Server {
	return &Server{cfg: cfg, secrets: secrets, backoff: initialBackoff}
}

// Enabled reports whether the server is enabled and configured sanely.
func (s *Server) Enabled() bool {
	if s.cfg.Disabled {
		return false
	}
	switch s.cfg.Type {
	case "remote":
		return s.cfg.URL != ""
	case "local":
		return len(s.cfg.Command) > 0
	}
	return false
}

// Name returns the config server name (tool name prefix).
func (s *Server) Name() string { return s.cfg.Name }

// client returns the live connection, dialing on demand. Failure applies
// the exponential backoff window (initial 1s, doubling to 30s).
func (s *Server) client(ctx context.Context) (*rpcClient, error) {
	s.mu.Lock()
	if s.rpc != nil {
		rpc := s.rpc
		s.mu.Unlock()
		return rpc, nil
	}
	if !s.lastDial.IsZero() {
		if wait := time.Until(s.lastDial.Add(s.backoff)); wait > 0 {
			s.mu.Unlock()
			return nil, fmt.Errorf("mcp %s: unavailable, retry in %s", s.cfg.Name, wait.Round(time.Millisecond))
		}
	}
	s.lastDial = time.Now()
	s.mu.Unlock()

	rpc, err := s.dialNow(ctx)
	if err != nil {
		s.mu.Lock()
		if s.backoff < maxBackoff {
			s.backoff *= 2
		}
		if !s.unavailable {
			s.unavailable = true
			log.Printf("mcp: server %q degraded: %v", s.cfg.Name, err)
		}
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Lock()
	s.rpc = rpc
	s.backoff = initialBackoff
	s.unavailable = false
	s.mu.Unlock()
	return rpc, nil
}

// dialNow builds the transport connection for the configured type.
func (s *Server) dialNow(ctx context.Context) (*rpcClient, error) {
	if s.dial != nil {
		return s.dial(ctx, s.cfg, s.secrets)
	}
	switch s.cfg.Type {
	case "remote":
		headers, err := resolveHeaders(s.cfg.Headers, s.secrets)
		if err != nil {
			return nil, fmt.Errorf("mcp %s: %w", s.cfg.Name, err)
		}
		return &rpcClient{
			call: (&remoteTransport{
				url:     s.cfg.URL,
				headers: headers,
				client:  &http.Client{}, // request contexts carry the deadlines
			}).call,
		}, nil
	case "local":
		t, err := startStdio(ctx, s.cfg.Command)
		if err != nil {
			return nil, fmt.Errorf("mcp %s: %w", s.cfg.Name, err)
		}
		return &rpcClient{call: t.call, close: t.shutdown}, nil
	default:
		return nil, fmt.Errorf("mcp %s: unknown type %q", s.cfg.Name, s.cfg.Type)
	}
}

// resolveHeaders resolves "secret:<name>" header values; a missing
// credential is an error and the raw value never leaks into the message.
func resolveHeaders(headers map[string]string, secrets Secrets) (map[string]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		if strings.Contains(v, "secret:") {
			if secrets == nil {
				return nil, fmt.Errorf("header %s: secret reference but no secret store", k)
			}
			resolved, err := secrets.ResolveSecrets(v)
			if err != nil {
				return nil, fmt.Errorf("header %s: %w", k, err)
			}
			out[k] = resolved
			continue
		}
		out[k] = v
	}
	return out, nil
}

func catalogTimeout(cfg ServerConfig) time.Duration {
	return toolTimeout(cfg.CatalogTimeout, defaultCatalogTimeout)
}

func executionTimeout(cfg ServerConfig) time.Duration {
	return toolTimeout(cfg.ExecutionTimeout, defaultExecutionTimeout)
}

// listTools fetches the tool catalog within catalog_timeout.
func (s *Server) listTools(ctx context.Context) ([]mcpTool, error) {
	rpc, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout(s.cfg))
	defer cancel()
	tools, err := rpc.listTools(ctx)
	if err != nil {
		s.drop(rpc)
		return nil, fmt.Errorf("mcp %s: tools/list: %w", s.cfg.Name, err)
	}
	return tools, nil
}

// callTool invokes one remote tool within execution_timeout. A server-side
// tool error is not a transport failure: it is surfaced via toolOutput.
func (s *Server) callTool(ctx context.Context, name string, input map[string]any) (toolOutput, error) {
	rpc, err := s.client(ctx)
	if err != nil {
		return toolOutput{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, executionTimeout(s.cfg))
	defer cancel()
	out, err := rpc.callTool(ctx, name, input)
	if err != nil {
		s.drop(rpc)
		return toolOutput{}, fmt.Errorf("mcp %s: %w", s.cfg.Name, err)
	}
	return out, nil
}

// drop discards a broken connection so the next call redials immediately
// (the connection died, not the server: backoff only gates failed dials).
func (s *Server) drop(rpc *rpcClient) {
	s.mu.Lock()
	if s.rpc == rpc {
		s.rpc = nil
		s.lastDial = time.Time{}
	}
	s.mu.Unlock()
	_ = rpc.shutdown()
}

// Available reports whether the server currently has a live connection.
func (s *Server) Available() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rpc != nil
}

// Close shuts down the live connection (kills a local child process).
func (s *Server) Close() error {
	s.mu.Lock()
	rpc := s.rpc
	s.rpc = nil
	s.mu.Unlock()
	if rpc != nil {
		return rpc.shutdown()
	}
	return nil
}
