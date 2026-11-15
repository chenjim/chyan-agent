// Scheduler: 1s-tick poller that fires due jobs through the run callback,
// skipping an occurrence while the same job is still running.
package cron

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Options configures a Scheduler. Run executes one job in its own
// goroutine and returns the deliverable output text (kept for ContextFrom
// injection into chained downstream prompts); its error is recorded in the
// job state. Now is the clock, injectable for tests; nil falls back to
// time.Now. Path, when set, persists runtime state (last run/status/error)
// to jobs.json after every run. OnRun, when set, receives the updated job
// after each run for SSE notification.
type Options struct {
	Run   func(ctx context.Context, j Job) (output string, err error)
	Now   func() time.Time
	Path  string
	OnRun func(j Job)
}

// Scheduler polls jobs once per second and fires each due job. Safe for
// concurrent use.
type Scheduler struct {
	opts Options

	mu      sync.Mutex
	saveMu  sync.Mutex // serializes jobs.json writes from runs and CRUD
	jobs    map[string]*Job
	nextAt  map[string]time.Time
	running map[string]bool
	outputs map[string]string // last deliverable output per job, for context_from
	wake    chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc // cancels in-flight runs on Stop
	wg      sync.WaitGroup     // tracks in-flight runs so Stop can wait
	stopped bool               // guards done and prevents Add after Stop begins waiting
}

// New creates an empty Scheduler. Add jobs with Add before Start.
func New(opts Options) *Scheduler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Scheduler{
		opts:    opts,
		jobs:    map[string]*Job{},
		nextAt:  map[string]time.Time{},
		running: map[string]bool{},
		outputs: map[string]string{},
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// Add registers jobs. Jobs with an unparsable Schedule are dropped so one
// bad job cannot break the schedule; event-only jobs (empty Schedule with
// After set) are kept and fire only from chaining. First fire is the next
// occurrence after now.
func (s *Scheduler) Add(jobs ...Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addLocked(jobs...)
}

func (s *Scheduler) addLocked(jobs ...Job) {
	now := s.opts.Now()
	for _, j := range jobs {
		if j.Schedule == "" && len(j.After) > 0 {
			j := j
			s.jobs[j.ID] = &j
			delete(s.nextAt, j.ID) // event-only: never due on the clock
			continue
		}
		sch, err := parseSchedule(j.Schedule)
		if err != nil {
			continue
		}
		j.sched = sch
		s.jobs[j.ID] = &j
		s.nextAt[j.ID] = sch.next(now)
	}
}

// Upsert adds or replaces one job and recomputes its next fire time. An
// in-flight run keeps running; the new definition applies to later ticks.
func (s *Scheduler) Upsert(j Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Preserve runtime state the caller did not supply (the Web form only
	// edits the definition, not last run fields).
	if old, ok := s.jobs[j.ID]; ok {
		if j.LastRunAt == nil {
			j.LastRunAt = old.LastRunAt
		}
		if j.LastStatus == "" {
			j.LastStatus = old.LastStatus
		}
		if j.LastError == "" {
			j.LastError = old.LastError
		}
		if j.LastSessionID == "" {
			j.LastSessionID = old.LastSessionID
		}
	}
	s.addLocked(j)
}

// Remove stops scheduling one job; an in-flight run is not interrupted.
func (s *Scheduler) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.jobs, id)
	delete(s.nextAt, id)
	delete(s.outputs, id)
	delete(s.running, id)
}

// SetLastSession records the session opened by a job's latest run so the
// Web can link back to it. Called by the run callback before it returns.
func (s *Scheduler) SetLastSession(id, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok {
		j.LastSessionID = sessionID
	}
}

// RunNow fires one job immediately regardless of its schedule. It reports
// false when the job is unknown or already running (occurrences never pile
// up), and the caller persists nothing: the run itself records the outcome.
func (s *Scheduler) RunNow(ctx context.Context, id string) (bool, error) {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return false, ErrJobNotFound
	}
	if !j.Enabled {
		s.mu.Unlock()
		return false, ErrJobDisabled
	}
	if s.running[id] {
		s.mu.Unlock()
		return false, nil
	}
	s.running[id] = true
	s.mu.Unlock()
	return s.spawn(ctx, j, map[string]bool{id: true}), nil
}

// Start begins the polling loop in the background and returns immediately.
// The loop exits when ctx is cancelled or Stop is called.
func (s *Scheduler) Start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	go s.loop(runCtx)
}

// Stop terminates the polling loop, cancels in-flight runs and waits for
// them to return. Idempotent.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		close(s.done)
	}
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.signal()
	s.wg.Wait()
}

// Save persists the current jobs to path. Writes are serialized with the
// scheduler's own run-time persistence so two writers cannot interleave.
func (s *Scheduler) Save(path string) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	return Save(path, s.Snapshot())
}

// Snapshot returns a copy of the current job states, including
// LastRunAt/LastStatus/LastError.
func (s *Scheduler) Snapshot() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, *j)
	}
	return out
}

// loop ticks once per second until stopped or ctx is cancelled.
func (s *Scheduler) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-s.wake:
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// signal wakes the loop so Stop takes effect without waiting a full tick.
func (s *Scheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// tick fires every job whose next fire time has arrived. An occurrence is
// dropped (skip, never pile up) while the job is disabled or still running
// from a previous tick.
func (s *Scheduler) tick(ctx context.Context) {
	now := s.opts.Now()
	s.mu.Lock()
	var due []*Job
	for id, j := range s.jobs {
		next := s.nextAt[id]
		if next.IsZero() || now.Before(next) {
			continue
		}
		if !j.Enabled || s.running[id] {
			s.nextAt[id] = j.sched.next(now) // drop this occurrence
			continue
		}
		s.running[id] = true
		s.nextAt[id] = j.sched.next(now) // advance before running
		due = append(due, j)
	}
	s.mu.Unlock()
	for _, j := range due {
		s.spawn(ctx, j, map[string]bool{j.ID: true})
	}
}

// spawn starts one run unless stopped and reports whether it started. Add and
// Stop's transition to waiting are serialized by mu so a zero-count Add cannot
// race with Wait.
func (s *Scheduler) spawn(ctx context.Context, j *Job, chain map[string]bool) bool {
	s.mu.Lock()
	if s.stopped {
		s.running[j.ID] = false
		s.mu.Unlock()
		return false
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go s.run(ctx, j, chain)
	return true
}

// run executes one job via the callback and records the outcome. On
// success it fires every enabled downstream job whose After lists this
// job id (event chaining, the drop-in replacement for hermes's
// "poll every 3m until upstream done" gate scripts). chain is the set of
// job ids already fired in this propagation: a cyclic A→B→A dependency
// runs each job once instead of ping-pong forever.
func (s *Scheduler) run(ctx context.Context, j *Job, chain map[string]bool) {
	defer s.wg.Done()
	defer s.recoverPanic(j)

	s.mu.Lock()
	call := *j
	call.Prompt = s.contextPromptLocked(*j)
	s.mu.Unlock()
	out, err := s.opts.Run(ctx, call)

	now := s.opts.Now()
	s.mu.Lock()
	t := now
	j.LastRunAt = &t
	if err != nil {
		j.LastStatus = "error"
		j.LastError = err.Error()
	} else {
		j.LastStatus = "ok"
		j.LastError = ""
		s.outputs[j.ID] = out
	}
	s.running[j.ID] = false
	var fired []*Job
	if err == nil {
		fired = s.dueDownstreamLocked(j.ID, chain)
	}
	updated := *j
	s.mu.Unlock()
	// Persist runtime state and notify subscribers after the lock is released:
	// disk IO and callbacks must not block scheduling.
	if s.opts.Path != "" {
		if serr := s.Save(s.opts.Path); serr != nil {
			log.Printf("cron: save %s: %v", s.opts.Path, serr)
		}
	}
	if s.opts.OnRun != nil {
		s.opts.OnRun(updated)
	}
	for _, d := range fired {
		next := make(map[string]bool, len(chain)+1)
		for id := range chain {
			next[id] = true
		}
		next[d.ID] = true
		s.spawn(ctx, d, next)
	}
}

// recoverPanic keeps one bad job from killing the whole daemon: the panic is
// recorded on the job state and its running flag is cleared.
func (s *Scheduler) recoverPanic(j *Job) {
	r := recover()
	if r == nil {
		return
	}
	now := s.opts.Now()
	s.mu.Lock()
	t := now
	j.LastRunAt = &t
	j.LastStatus = "error"
	j.LastError = fmt.Sprintf("panic: %v", r)
	s.running[j.ID] = false
	s.mu.Unlock()
}

// dueDownstreamLocked returns enabled, idle downstream jobs of upstream and
// marks them running so their fire is not double-scheduled. Jobs already in
// the propagation chain are skipped (cycle guard). Callers hold s.mu.
func (s *Scheduler) dueDownstreamLocked(upstream string, chain map[string]bool) []*Job {
	var out []*Job
	for id, j := range s.jobs {
		if !containsID(j.After, upstream) || !j.Enabled || s.running[id] || chain[id] {
			continue
		}
		s.running[id] = true
		out = append(out, j)
	}
	return out
}

// contextPromptLocked prepends the outputs of the jobs listed in
// ContextFrom to the prompt ("## Upstream Output" block, hermes
// _inject_context_from semantics: the previous run's answer, not its
// prompt, is the context). Callers hold s.mu.
func (s *Scheduler) contextPromptLocked(j Job) string {
	if len(j.ContextFrom) == 0 {
		return j.Prompt
	}
	var b strings.Builder
	for _, up := range j.ContextFrom {
		out := s.outputs[up]
		if out == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("## Upstream Output\n")
		b.WriteString("The following is the most recent output of job " + up + ". Use it as context.\n\n```\n")
		b.WriteString(out)
		b.WriteString("\n```\n")
	}
	if b.Len() == 0 {
		return j.Prompt
	}
	return b.String() + "\n" + j.Prompt
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
