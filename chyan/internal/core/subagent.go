package core

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"chyan.tech/chyan/internal/trace"
)

// SubAgentConfig bounds sub-agent dispatch (architecture 4.1.1).
type SubAgentConfig struct {
	// MaxDepth is the nesting cap (<= 2: a sub-agent spawns no children).
	MaxDepth int
	// MaxConcurrent caps parallel sub-agents per parent.
	MaxConcurrent int
	// Timeout bounds one sub-agent run.
	Timeout time.Duration
}

// SubAgentDeps wires a sub-agent runner to its factories.
type SubAgentDeps struct {
	// NewAgent builds a child agent for one dispatch. The child gets its
	// own session (parent_id linked) and its own context manager.
	NewAgent func(ctx context.Context, req TaskRequest, parent AgentContext) (*AgentImpl, error)
	// MaxToolIterations caps the child's tool loop (tighter than the parent).
	MaxToolIterations int
}

// AgentContext carries one parent's effective run settings into a child.
// Perms is the action-level permission subset (nil = unconstrained); Model is
// the effective parent model. ModelOverride keeps an explicit --model ahead
// of a child agent's own model declaration.
type AgentContext struct {
	SessionID     string
	Depth         int
	Perms         map[string]Effect
	Model         string
	ModelOverride string
	// WorkDir is the parent run's workspace root; children inherit it so a
	// sub-agent never escapes the selected project sandbox.
	WorkDir string
}

// SubAgentRunner dispatches child agents with nesting/concurrency limits.
type SubAgentRunner struct {
	cfg  SubAgentConfig
	deps SubAgentDeps

	mu     sync.Mutex
	active int
	depth  int
	cancel context.CancelFunc
	// tracer records dispatch spans so a child's model/tool cost rolls up to
	// the parent turn (FR-13). Nil disables persistence.
	tracer *trace.Recorder
}

// SetTracer wires span recording for sub-agent dispatches.
func (r *SubAgentRunner) SetTracer(rec *trace.Recorder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tracer = rec
}

// NewSubAgentRunner builds a runner.
func NewSubAgentRunner(cfg SubAgentConfig, deps SubAgentDeps) *SubAgentRunner {
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = 2
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if deps.MaxToolIterations <= 0 {
		deps.MaxToolIterations = 15
	}
	return &SubAgentRunner{cfg: cfg, deps: deps}
}

// ErrNestingTooDeep is returned when a dispatch would exceed MaxDepth.
var ErrNestingTooDeep = errors.New("core: sub-agent nesting depth exceeded")

// ErrTooManySubAgents is returned when the concurrency cap is hit.
var ErrTooManySubAgents = errors.New("core: too many concurrent sub-agents")

// Dispatch spawns one child agent and returns its result summary. The child
// runs on its own session; only the summary comes back (context isolation,
// architecture 4.1.1).
func (r *SubAgentRunner) Dispatch(ctx context.Context, req TaskRequest, parent AgentContext) (TaskResult, error) {
	if parent.Depth >= r.cfg.MaxDepth {
		return TaskResult{}, ErrNestingTooDeep
	}
	if !r.acquire() {
		return TaskResult{}, ErrTooManySubAgents
	}
	defer r.release()

	child := parent
	child.Depth = parent.Depth + 1

	runCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	agent, err := r.deps.NewAgent(runCtx, req, child)
	if err != nil {
		return TaskResult{Success: false, Err: err.Error()}, err
	}

	name := req.AgentName
	if name == "" {
		name = "anonymous"
	}
	r.mu.Lock()
	tracer := r.tracer
	r.mu.Unlock()
	runCtx, span := trace.Start(runCtx, tracer, trace.KindSubagent, name, map[string]any{
		"agent": name, "depth": child.Depth, "session_id": agent.session,
	})
	result, err := agent.Run(runCtx, req.Prompt)
	if span != nil {
		span.SetAttr("success", err == nil)
		span.End(err)
	}
	if err != nil {
		return TaskResult{Success: false, Err: err.Error()}, err
	}
	// Context isolation: only the summary crosses back, never the transcript.
	// The session id must be the child's own (read_session target), not the
	// parent's copy carried over by AgentContext.
	summary := FlattenText(result)
	return TaskResult{
		SessionID: agent.session,
		Success:   true,
		Summary:   summary,
	}, nil
}

// DispatchParallel spawns several children concurrently and returns results
// in completion order (architecture 4.1.1).
func (r *SubAgentRunner) DispatchParallel(ctx context.Context, reqs []TaskRequest, parent AgentContext) []TaskResult {
	out := make([]TaskResult, len(reqs))
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Add(1)
		go func(i int, req TaskRequest) {
			defer wg.Done()
			res, err := r.Dispatch(ctx, req, parent)
			if err != nil && res.Err == "" {
				res.Err = err.Error()
			}
			out[i] = res
		}(i, req)
	}
	wg.Wait()
	return out
}

// Interrupt cancels all in-flight sub-agent runs.
func (r *SubAgentRunner) Interrupt() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
}

func (r *SubAgentRunner) acquire() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active >= r.cfg.MaxConcurrent {
		return false
	}
	r.active++
	return true
}

func (r *SubAgentRunner) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active--
}

// SummarizeChild renders a child result for parent injection: the summary
// text plus a pointer to the full transcript (read_session), never the
// transcript itself.
func SummarizeChild(res TaskResult) string {
	if res.Err != "" {
		return fmt.Sprintf("[子 Agent 失败] %s", res.Err)
	}
	var b strings.Builder
	b.WriteString("[子 Agent 结果]")
	if res.Summary != "" {
		b.WriteString("\n")
		b.WriteString(res.Summary)
	}
	if res.SessionID != "" {
		b.WriteString("\n完整过程: read_session(")
		b.WriteString(res.SessionID)
		b.WriteString(")")
	}
	return b.String()
}

// logSubAgentStart is a thin observability hook for child dispatch.
func logSubAgentStart(req TaskRequest, depth int) {
	name := req.AgentName
	if name == "" {
		name = "anonymous"
	}
	log.Printf("core: dispatch sub-agent %s at depth %d", name, depth)
}
