// Package tui is the bubbletea desktop client for the chyan daemon. It talks
// to the daemon over HTTP (REST + SSE, api contract) — it never touches the
// core or store layers directly.
package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"chyan.tech/chyan/internal/api"
)

// paths is the frozen REST surface (api package, base /api/v1). The stream
// path is owned by the daemon (P3-1) and shared with the PWA client.
const (
	pathSessions  = "/api/v1/sessions"
	pathTasks     = "/api/v1/tasks"
	pathApprovals = "/api/v1/approvals"
	pathStatus    = "/api/v1/status"
	pathCommands  = "/api/v1/commands"
	pathStream    = "/api/v1/events"
	pathReload    = "/api/v1/config/reload"
	pathHealth    = "/healthz"
)

// Event is one decoded SSE frame. Data holds the event payload when it is a
// JSON object (task.* events carry an api.Task), nil otherwise.
type Event struct {
	Name string
	Data map[string]any
}

// IsTask reports whether the event belongs to the task.* family.
func (e Event) IsTask() bool { return strings.HasPrefix(e.Name, "task.") }

// Task decodes the api.Task payload of a task.* event. It returns an error
// when Data is absent or not a task payload.
func (e Event) Task() (api.Task, error) {
	if e.Data == nil {
		return api.Task{}, fmt.Errorf("tui: event %s has no task payload", e.Name)
	}
	raw, err := json.Marshal(e.Data)
	if err != nil {
		return api.Task{}, err
	}
	var t api.Task
	if err := json.Unmarshal(raw, &t); err != nil {
		return api.Task{}, err
	}
	return t, nil
}

// TaskOutput returns the incremental output text of a task.output event
// (payload shape owned by the daemon; "text" first, "output" as fallback).
func (e Event) TaskOutput() string {
	for _, key := range []string{"text", "output"} {
		if s, ok := e.Data[key].(string); ok {
			return s
		}
	}
	return ""
}

// Client talks to one chyan daemon. A nil *Client is valid and makes every
// call fail with ErrUnavailable, so the UI can start before a daemon exists.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	// stream has no request timeout: SSE connections are long-lived and are
	// bounded by context cancellation instead.
	stream *http.Client
}

// ErrUnavailable is returned by all Client calls when the client is nil.
var ErrUnavailable = fmt.Errorf("daemon unavailable")

// NewClient builds a client for the daemon at baseURL (http://host:port or
// http://unix/<socket-path>). token is sent as a Bearer token when non-empty;
// unix-socket connections trust the OS user and need no token. The socket
// path is consumed by the dialer only: request URLs are http://unix/api/...,
// never embedding the socket path in the HTTP request line.
func NewClient(baseURL, token string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	transport := http.DefaultTransport
	if sock, ok := unixSocketOf(baseURL); ok {
		baseURL = "http://unix"
		d := net.Dialer{}
		transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return d.DialContext(ctx, "unix", sock)
			},
		}
	}
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second, Transport: transport},
		stream:  &http.Client{Transport: transport},
	}
}

// unixSocketOf maps "http://unix/<path>" to "<path>" (socket file, leading
// slash preserved); ok is false for plain http(s) URLs.
func unixSocketOf(baseURL string) (string, bool) {
	path, ok := strings.CutPrefix(baseURL, "http://unix")
	if !ok {
		return "", false
	}
	if path == "" || path == "/" {
		return "", false
	}
	return path, true
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	if c == nil {
		return ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResponse(resp, out)
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

// do issues one request with an optional JSON body and decodes the reply.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if c == nil {
		return ErrUnavailable
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResponse(resp, out)
}

func (c *Client) auth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// decodeResponse decodes a JSON success body into out (optional) and maps
// non-2xx responses to the api.Error envelope.
func decodeResponse(resp *http.Response, out any) error {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr api.Error
		if json.Unmarshal(raw, &apiErr) == nil && apiErr.Error != "" {
			return fmt.Errorf("daemon %s: %s", resp.Status, apiErr.Error)
		}
		return fmt.Errorf("daemon %s", resp.Status)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Ping probes the daemon (GET /healthz, no auth). A short-lived context makes
// it usable as a readiness check while starting the daemon.
func (c *Client) Ping(ctx context.Context) error {
	return c.get(ctx, pathHealth, nil)
}

// ListSessions returns recent sessions, newest first.
func (c *Client) ListSessions(ctx context.Context) ([]api.SessionSummary, error) {
	var list []api.SessionSummary
	if err := c.get(ctx, pathSessions, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// ListTasks returns the daemon's live background tasks.
func (c *Client) ListTasks(ctx context.Context) ([]api.Task, error) {
	var list []api.Task
	if err := c.get(ctx, pathTasks, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// GetTask returns one task's current state (polling fallback when the SSE
// stream breaks).
func (c *Client) GetTask(ctx context.Context, id string) (api.Task, error) {
	var t api.Task
	if err := c.get(ctx, pathTasks+"/"+url.PathEscape(id), &t); err != nil {
		return api.Task{}, err
	}
	return t, nil
}

// CreateSession starts an empty conversation and returns its summary. The CLI
// uses it before the first turn (shell notes, /status) so the daemon owns the
// session row instead of opening the database itself.
func (c *Client) CreateSession(ctx context.Context, req api.CreateSessionRequest) (api.SessionSummary, error) {
	var s api.SessionSummary
	if err := c.post(ctx, pathSessions, req, &s); err != nil {
		return api.SessionSummary{}, err
	}
	return s, nil
}

// SessionContext returns one session's live window usage.
func (c *Client) SessionContext(ctx context.Context, id string) (api.ContextUsage, error) {
	var b api.ContextUsage
	if err := c.get(ctx, pathSessions+"/"+url.PathEscape(id)+"/context", &b); err != nil {
		return api.ContextUsage{}, err
	}
	return b, nil
}

// CompactSession folds one session's history into a handoff note
// (POST /api/v1/sessions/{id}/compact). The id may be "continue".
func (c *Client) CompactSession(ctx context.Context, id string) (api.HandoffNote, error) {
	var res api.CompactResult
	if err := c.post(ctx, pathSessions+"/"+url.PathEscape(id)+"/compact", nil, &res); err != nil {
		return api.HandoffNote{}, err
	}
	return res.Note, nil
}

// AppendNote records a user-run shell command and its output as a user message
// (POST /api/v1/sessions/{id}/note); the id may be "continue".
func (c *Client) AppendNote(ctx context.Context, id, command, output string) (string, error) {
	var res api.AppendNoteResult
	body := api.AppendNoteRequest{Command: command, Output: output}
	if err := c.post(ctx, pathSessions+"/"+url.PathEscape(id)+"/note", body, &res); err != nil {
		return "", err
	}
	return res.SessionID, nil
}

// ListCommands returns the project slash commands for help and completion.
func (c *Client) ListCommands(ctx context.Context) ([]api.CommandInfo, error) {
	var list []api.CommandInfo
	if err := c.get(ctx, pathCommands, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// GetSession returns one session with its transcript.
func (c *Client) GetSession(ctx context.Context, id string) (api.Session, error) {
	return c.SessionTranscript(ctx, id, 0, false)
}

// SessionTranscript returns a bounded transcript. limit<=0 keeps the daemon
// default (500); tail selects the newest rows (session-switch replay) instead
// of the oldest (title fallback).
func (c *Client) SessionTranscript(ctx context.Context, id string, limit int, tail bool) (api.Session, error) {
	path := pathSessions + "/" + url.PathEscape(id)
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if tail {
		q.Set("tail", "1")
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var s api.Session
	if err := c.get(ctx, path, &s); err != nil {
		return api.Session{}, err
	}
	return s, nil
}

// RenameSession sets one session title (PATCH /api/v1/sessions/{id}).
// The daemon caps titles at 20 runes and rejects longer ones.
func (c *Client) RenameSession(ctx context.Context, id, title string) (api.SessionSummary, error) {
	var s api.SessionSummary
	if err := c.do(ctx, http.MethodPatch, pathSessions+"/"+url.PathEscape(id),
		api.UpdateSessionRequest{Title: title}, &s); err != nil {
		return api.SessionSummary{}, err
	}
	return s, nil
}

// DeleteSession removes one session and its messages
// (DELETE /api/v1/sessions/{id}).
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, pathSessions+"/"+url.PathEscape(id), nil, nil)
}

// ExportSession downloads the portable JSON envelope of one session
// (GET /api/v1/sessions/{id}/export), identical to `chyan export`.
func (c *Client) ExportSession(ctx context.Context, id string) ([]byte, error) {
	if c == nil {
		return nil, ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+pathSessions+"/"+url.PathEscape(id)+"/export", nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeResponse(resp, nil)
	}
	return io.ReadAll(resp.Body)
}

// CreateTask starts one background agent run and returns the created task.
func (c *Client) CreateTask(ctx context.Context, req api.CreateTaskRequest) (api.Task, error) {
	var t api.Task
	if err := c.post(ctx, pathTasks, req, &t); err != nil {
		return api.Task{}, err
	}
	return t, nil
}

// CancelTask asks the daemon to interrupt a running task. Idempotent: a task
// that already finished comes back with its terminal status.
func (c *Client) CancelTask(ctx context.Context, id string) (api.Task, error) {
	var t api.Task
	if err := c.post(ctx, pathTasks+"/"+url.PathEscape(id)+"/cancel", nil, &t); err != nil {
		return api.Task{}, err
	}
	return t, nil
}

// ListApprovals returns pending approval tickets.
func (c *Client) ListApprovals(ctx context.Context) ([]api.ApprovalTicket, error) {
	var list []api.ApprovalTicket
	if err := c.get(ctx, pathApprovals, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// DecideApproval resolves one ticket (approve|deny).
func (c *Client) DecideApproval(ctx context.Context, id string, d api.ApprovalDecision) error {
	return c.post(ctx, pathApprovals+"/"+url.PathEscape(id), d, nil)
}

// GetStatus returns daemon-wide observability counters.
func (c *Client) GetStatus(ctx context.Context) (api.Status, error) {
	var s api.Status
	if err := c.get(ctx, pathStatus, &s); err != nil {
		return api.Status{}, err
	}
	return s, nil
}

// ReloadConfig makes the daemon re-read its config.toml. It fails with a
// conflict while a turn is running: the daemon refuses instead of waiting.
func (c *Client) ReloadConfig(ctx context.Context) (api.ReloadResult, error) {
	var res api.ReloadResult
	if err := c.post(ctx, pathReload, nil, &res); err != nil {
		return api.ReloadResult{}, err
	}
	return res, nil
}

// Events subscribes to the daemon SSE stream. It emits events on out until
// ctx is canceled or the connection breaks (then the error is returned). The
// caller owns out; the channel is never closed here.
func (c *Client) Events(ctx context.Context, out chan<- Event) error {
	return c.EventsWithReady(ctx, out, nil)
}

// EventsWithReady is Events with a readiness callback invoked once the stream
// response is accepted. The CLI subscribes before creating a task, so no early
// token deltas are lost.
func (c *Client) EventsWithReady(ctx context.Context, out chan<- Event, ready func()) error {
	if c == nil {
		return ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+pathStream, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.auth(req)
	resp, err := c.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeResponse(resp, nil)
	}
	if ready != nil {
		ready()
	}
	return parseSSE(ctx, resp.Body, out)
}

// parseSSE decodes the SSE wire format (event:/data: lines, blank-line frame
// separator, ":" comments ignored). Multi-line data is joined with "\n". A
// trailing frame without a final blank line is emitted at stream end.
func parseSSE(ctx context.Context, r io.Reader, out chan<- Event) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var name string
	var data []string
	emit := func() bool {
		if name == "" && len(data) == 0 {
			return true
		}
		ev := Event{Name: name, Data: parseEventData(strings.Join(data, "\n"))}
		name, data = "", nil
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if !emit() {
				return ctx.Err()
			}
		case strings.HasPrefix(line, ":"):
			// comment / keepalive
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !emit() {
		return ctx.Err()
	}
	return nil
}

// parseEventData decodes a JSON object payload; non-object payloads yield nil.
func parseEventData(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}
