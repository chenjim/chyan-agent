// Package daemon serves the chyan HTTP API on two listeners (architecture
// 4.5): a local unix-domain socket trusting the OS user, and a TCP listener
// guarded by a bearer token for remote access (Tailscale / reverse proxy).
package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/cron"
	"chyan.tech/chyan/internal/store"
	"chyan.tech/chyan/web"
)

// Options configure the daemon listeners and auth.
type Options struct {
	// Home is the chyan data root (default ~/.chyan): the unix socket lives
	// at <Home>/chyan.sock. The auth token is never persisted here; it comes
	// from --token / CHYAN_TOKEN.
	Home string
	// TCPAddr is the remote listener address (default 127.0.0.1:31415).
	TCPAddr string
	// Token authenticates the TCP listener; empty disables the TCP listener
	// (local socket only).
	Token string
	// App is the wired agent stack to serve.
	App *app.App
	// Store persists tasks/sessions; defaults to App's own store via API.
	Store store.Store
	// Gate resolves ask-effect requests (nil = auto-approve, P2 behaviour).
	Gate ApprovalGate
	// MaxConcurrentTasks caps background dispatches (default 4). Extra tasks
	// wait in the pending state with a slot in FIFO-ish order.
	MaxConcurrentTasks int
	// TaskTimeout bounds one dispatch's run time (default 30m). It starts
	// when the task leaves the queue, not when it is created.
	TaskTimeout time.Duration
	// WatchdogInterval is the self-healing scan period (default 10m; negative
	// disables the watchdog).
	WatchdogInterval time.Duration
}

// Daemon defaults (architecture appendix A).
const (
	defaultMaxConcurrentTasks = 4
	defaultTaskTimeout        = 30 * time.Minute
	defaultWatchdogInterval   = 10 * time.Minute
)

// ApprovalGate resolves pending ask-effect tickets over the remote API. The
// production implementation is *ApprovalHub (approvals.go), which also
// implements core.Approver; tests inject fakes.
type ApprovalGate interface {
	// Pending returns a creation-ordered snapshot of unresolved tickets.
	Pending() []api.ApprovalTicket
	// Resolve delivers a human decision to the blocked approver. Unknown or
	// already-resolved ids return an error (HTTP 400).
	Resolve(id, decision, reason string) error
}

// Server is the daemon core: one app, two listeners, one event hub.
type Server struct {
	app   *app.App
	store store.Store
	hub   *hub
	gate  ApprovalGate
	// imApprover resolves approvals from IM decision replies; nil when no
	// channel registry or a non-hub gate (wireIMApprover).
	imApprover *IMApprover
	// inbound dispatches whitelisted IM messages to agent runs; nil when no
	// channel registry is wired.
	inbound *inboundState
	// router is the optional always-on dispatch layer (P1-5); nil unless
	// CHYAN_ROUTER is set and inbound is wired.
	router *router
	// cronSched is the scheduler the Web cron views manage; nil until
	// SetScheduler is called (tests may leave it nil, file-only mode).
	cronSched *cron.Scheduler

	token   string
	tcpAddr string
	sockDir string

	// taskSlots caps concurrent dispatches; queued tasks hold a goroutine
	// blocked here and stay "pending" until they acquire a slot.
	taskSlots        chan struct{}
	taskTimeout      time.Duration
	watchdogInterval time.Duration
	startedAt        time.Time

	tasksMu      sync.Mutex
	tasks        map[string]*taskState
	lastWatchdog time.Time

	// reloadMu serializes config reloads (POST /api/v1/config/reload).
	reloadMu sync.Mutex

	tasksWG sync.WaitGroup // background dispatches, drained on shutdown
	ctxMu   sync.RWMutex   // guards runCtx
	runCtx  context.Context

	srvHTTP  *http.Server
	srvLocal *http.Server
}

// daemonContext returns the daemon lifetime context (background before Start
// or in tests), for work that outlives one HTTP request.
func (s *Server) daemonContext() context.Context {
	s.ctxMu.RLock()
	defer s.ctxMu.RUnlock()
	if s.runCtx != nil {
		return s.runCtx
	}
	return context.Background()
}

// taskContext derives one background task context from the daemon lifetime,
// bounded by the configured per-task timeout.
func (s *Server) taskContext() (context.Context, context.CancelFunc) {
	s.ctxMu.RLock()
	base := s.runCtx
	s.ctxMu.RUnlock()
	if base == nil {
		base = context.Background()
	}
	return context.WithTimeout(base, s.taskTimeout)
}

// baseContext is the daemon lifetime without the per-task timeout: queue
// waits are bounded by shutdown only, not by the execution budget.
func (s *Server) baseContext() context.Context {
	s.ctxMu.RLock()
	defer s.ctxMu.RUnlock()
	if s.runCtx == nil {
		return context.Background()
	}
	return s.runCtx
}

// acquireTaskSlot blocks until a dispatch slot is free or the daemon stops.
func (s *Server) acquireTaskSlot() bool {
	select {
	case s.taskSlots <- struct{}{}:
		return true
	case <-s.baseContext().Done():
		return false
	}
}

// releaseTaskSlot frees one dispatch slot.
func (s *Server) releaseTaskSlot() { <-s.taskSlots }

// recoverInterruptedTasks marks dispatches left pending/running by a previous
// process as interrupted. Cron rows are owned by the scheduler and skipped:
// their status is rewritten on the next fire.
func (s *Server) recoverInterruptedTasks(ctx context.Context) {
	for _, status := range []string{"running", "pending"} {
		rows, err := s.store.ListTasks(ctx, status)
		if err != nil {
			log.Printf("daemon: recover %s tasks: %v", status, err)
			continue
		}
		for _, t := range rows {
			if t.Kind == "cron" {
				continue
			}
			t.Status = "interrupted"
			if err := s.store.UpdateTask(ctx, t); err != nil {
				log.Printf("daemon: mark task %s interrupted: %v", t.ID, err)
				continue
			}
			log.Printf("daemon: task %s recovered as interrupted", t.ID)
		}
	}
}

// New builds a daemon server from options.
func New(opts Options) (*Server, error) {
	if opts.App == nil {
		return nil, errors.New("daemon: App is required")
	}
	if opts.Store == nil {
		return nil, errors.New("daemon: Store is required")
	}
	home := opts.Home
	if home == "" {
		return nil, errors.New("daemon: Home is required")
	}
	tcpAddr := opts.TCPAddr
	if tcpAddr == "" {
		tcpAddr = "127.0.0.1:31415"
	}
	maxTasks := opts.MaxConcurrentTasks
	if maxTasks <= 0 {
		maxTasks = defaultMaxConcurrentTasks
	}
	taskTimeout := opts.TaskTimeout
	if taskTimeout <= 0 {
		taskTimeout = defaultTaskTimeout
	}
	watchdog := opts.WatchdogInterval
	if watchdog == 0 {
		watchdog = defaultWatchdogInterval
	}
	s := &Server{
		app: opts.App, store: opts.Store, hub: newHub(), gate: opts.Gate,
		token: opts.Token, tcpAddr: tcpAddr, sockDir: home,
		taskSlots: make(chan struct{}, maxTasks), taskTimeout: taskTimeout,
		watchdogInterval: watchdog, startedAt: time.Now().UTC(),
		tasks: map[string]*taskState{},
	}
	// Push approval events to SSE clients when the gate is the production hub.
	if h, ok := opts.Gate.(*ApprovalHub); ok {
		h.SetNotify(s.hub.publish)
	}
	// Production hub + channel registry: approvals can also be resolved from
	// IM decision replies (auto-wrap, Options unchanged).
	s.wireIMApprover(opts)
	if reg := opts.App.Channels(); reg != nil {
		s.inbound = newInboundState(s, reg.InboundPolicies())
		if routerEnabled() {
			s.router = &router{server: s}
			log.Printf("daemon: router layer enabled (%s)", RouterEnv)
		}
	}
	return s, nil
}

// SetScheduler attaches the cron scheduler the Web views manage. Call it
// once before Start: the scheduler drives jobs and the daemon only exposes
// them. Without it the cron endpoints fall back to jobs.json (read-only for
// run/toggle semantics), which keeps tests and CLI-only setups working.
func (s *Server) SetScheduler(sched *cron.Scheduler) { s.cronSched = sched }

// CronRun publish hook: the scheduler calls it after each job run so the
// PWA can update the job status and link the new session without polling.
func (s *Server) CronRun(j cron.Job) {
	s.hub.publish(Event{Name: api.EventCronRun, Data: j})
}

// Handler returns the REST router (used by both listeners and tests).
// Non-API paths serve the embedded PWA (architecture 4.5: daemon hosts the
// phone UI, no separate static server).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/tasks", s.handleCreateTask)
	mux.HandleFunc("GET /api/v1/tasks", s.handleListTasks)
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.handleGetTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/cancel", s.handleCancelTask)
	mux.HandleFunc("GET /api/v1/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/v1/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/compact", s.handleCompactSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/note", s.handleAppendNote)
	mux.HandleFunc("PATCH /api/v1/sessions/{id}", s.handleUpdateSession)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("GET /api/v1/sessions/{id}/export", s.handleExportSession)
	mux.HandleFunc("GET /api/v1/sessions/{id}/context", s.handleSessionContext)
	mux.HandleFunc("GET /api/v1/search", s.handleSearch)
	mux.HandleFunc("GET /api/v1/traces", s.handleTraces)
	mux.HandleFunc("POST /api/v1/config/reload", s.handleReloadConfig)
	mux.HandleFunc("GET /api/v1/approvals", s.handleListApprovals)
	mux.HandleFunc("POST /api/v1/approvals/{id}", s.handleResolveApproval)
	mux.HandleFunc("GET /api/v1/files", s.handleListFiles)
	mux.HandleFunc("GET /api/v1/files/content", s.handleFileContent)
	mux.HandleFunc("GET /api/v1/files/download", s.handleFileDownload)
	mux.HandleFunc("GET /api/v1/workspace", s.handleWorkspace)
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/commands", s.handleListCommands)
	mux.HandleFunc("GET /api/v1/cron", s.handleListCron)
	mux.HandleFunc("POST /api/v1/cron", s.handleCreateCron)
	mux.HandleFunc("PATCH /api/v1/cron/{id}", s.handleUpdateCron)
	mux.HandleFunc("DELETE /api/v1/cron/{id}", s.handleDeleteCron)
	mux.HandleFunc("POST /api/v1/cron/{id}/run", s.handleRunCron)
	mux.HandleFunc("GET /api/v1/memory", s.handleListMemory)
	mux.HandleFunc("GET /api/v1/memory/dream", s.handleGetDream)
	mux.HandleFunc("POST /api/v1/memory/dream", s.handleRunDream)
	mux.HandleFunc("POST /api/v1/memory/{id}/supersede", s.handleSupersedeMemory)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/events", s.serveSSEAuthed)
	mux.HandleFunc("POST /api/v1/channels/feishu/webhook", s.handleFeishuWebhook)
	mux.Handle("/", web.Handler())
	return mux
}

// Start binds both listeners and serves until ctx is cancelled. It blocks.
func (s *Server) Start(ctx context.Context) error {
	// Claim the data root before touching task rows: a second instance must not
	// unlink the live socket or mark the first instance's tasks interrupted.
	lock, err := acquireHomeLock(s.sockDir)
	if err != nil {
		return err
	}
	if lock != nil {
		defer lock.Close()
	}

	errCh := make(chan error, 2)
	s.ctxMu.Lock()
	s.runCtx = ctx
	s.ctxMu.Unlock()

	// Tasks left "running" by a crash or a hard kill can never finish: mark
	// them interrupted before serving so the UI never shows a zombie.
	s.recoverInterruptedTasks(ctx)
	log.Printf("daemon: %d task slots, %s per-task timeout", cap(s.taskSlots), s.taskTimeout)

	// Inbound IM messages flow to the SSE hub for TUI/PWA (architecture 4.5).
	go s.bridgeChannelUpdates(ctx)
	// Self-healing scan: stalled dispatches are interrupted and stale sessions
	// are closed instead of looking alive forever in the UI.
	go s.watchdogLoop(ctx)

	// Local listener: unix-domain socket. Windows has no local face (D30):
	// remote access is TCP + token, so refusing to start without a token is
	// clearer than silently serving nothing.
	sockPath := filepath.Join(s.sockDir, "chyan.sock")
	if runtime.GOOS == "windows" {
		if s.token == "" {
			return errors.New("daemon: Windows requires --token (no local socket face, see D30)")
		}
		sockPath = ""
		log.Printf("daemon: local listener disabled on Windows (TCP + token only)")
	} else {
		_ = os.Remove(sockPath)
		lc := net.ListenConfig{}
		localLis, err := lc.Listen(ctx, "unix", sockPath)
		if err != nil {
			return fmt.Errorf("daemon: listen unix %s: %w", sockPath, err)
		}
		s.srvLocal = &http.Server{
			Handler:           s.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			if err := s.srvLocal.Serve(localLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
		log.Printf("daemon: local listener %s", sockPath)
	}

	// Remote listener: TCP + bearer token.
	if s.token != "" {
		tcpLis, err := net.Listen("tcp", s.tcpAddr)
		if err != nil {
			return fmt.Errorf("daemon: listen tcp %s: %w", s.tcpAddr, err)
		}
		s.srvHTTP = &http.Server{
			Handler:           s.authMiddleware(s.Handler()),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			if err := s.srvHTTP.Serve(tcpLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
		log.Printf("daemon: remote listener %s (token auth)", s.tcpAddr)
	} else {
		log.Printf("daemon: remote listener disabled (no token)")
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if s.srvHTTP != nil {
			_ = s.srvHTTP.Shutdown(shutdownCtx)
		}
		if s.srvLocal != nil {
			_ = s.srvLocal.Shutdown(shutdownCtx)
		}
		// Background dispatches inherit ctx and return promptly; queued ones
		// leave acquireTaskSlot. Give them a bounded drain so a shutdown does
		// not truncate a stored task result.
		drained := make(chan struct{})
		go func() {
			s.tasksWG.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-shutdownCtx.Done():
			log.Printf("daemon: shutdown drain timed out, tasks still running")
		}
		if sockPath != "" {
			_ = os.Remove(sockPath)
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// authMiddleware enforces bearer-token auth on the TCP listener. The unix
// socket is reachable only by local OS users, so it skips this. Non-/api/
// paths (the embedded PWA shell) stay open: a browser cannot attach an
// Authorization header to the initial asset loads, and the shell carries no
// data — every data call is under /api/ and stays guarded.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, prefix)), []byte(s.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveSSEAuthed wraps the SSE handler for the REST mux.
func (s *Server) serveSSEAuthed(w http.ResponseWriter, r *http.Request) {
	s.hub.serveSSE(w, r)
}

// ------------------------------------------------------------- JSON helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("daemon: encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, api.Error{Error: msg})
}

// maxBodyBytes caps JSON request bodies (tasks carry a prompt, approvals a
// verdict): an unbounded decoder lets one client exhaust memory.
const maxBodyBytes = 1 << 20

func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	return decodeBodyLimit(w, r, v, maxBodyBytes)
}

// decodeBodyLimit is decodeBody with a per-route cap (task uploads are larger
// than approval/task-control payloads).
func decodeBodyLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v)
}
