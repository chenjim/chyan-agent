// Tests for schedule parsing, next-fire computation, load/save and the scheduler.
package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		t.Fatalf("bad time %q: %v", s, err)
	}
	return tm
}

func TestParseScheduleValid(t *testing.T) {
	for _, expr := range []string{
		"* * * * *",
		"*/15 * * * *",
		"0 0 * * *",
		"38 14 * * 1-5",
		"0 9,18 * * 1,3,5",
		"0 0 1,15 * *",
		"0 0 1 1-6 *",
		"0 0 29 2 *",
		"5 4 * * 7",
		"30 4 1,15 * 5",
		"0 0 * * 0-6",
		"@every 30s",
		"@every 2h30m",
	} {
		if _, err := parseSchedule(expr); err != nil {
			t.Errorf("parseSchedule(%q): unexpected error %v", expr, err)
		}
	}
}

func TestParseScheduleInvalid(t *testing.T) {
	for _, expr := range []string{
		"",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * 32 * *",
		"* * * 13 *",
		"* * * * 8",
		"*/0 * * * *",
		"1-2-3 * * * *",
		"abc * * * *",
		"@every",
		"@every 0s",
		"@every -5s",
		"@every xyz",
	} {
		if _, err := parseSchedule(expr); err == nil {
			t.Errorf("parseSchedule(%q): want error, got nil", expr)
		}
	}
}

func TestScheduleNext(t *testing.T) {
	cases := []struct {
		expr string
		from string
		want string
	}{
		{"@every 30s", "2026-01-01 10:07:33", "2026-01-01 10:08:03"},
		{"*/15 * * * *", "2026-01-01 10:07:33", "2026-01-01 10:15:00"},
		{"*/15 * * * *", "2026-01-01 10:15:00", "2026-01-01 10:30:00"}, // strictly after
		{"0 0 * * *", "2026-01-01 10:07:00", "2026-01-02 00:00:00"},
		{"38 14 * * 1-5", "2026-01-01 15:00:00", "2026-01-02 14:38:00"}, // Thu -> Fri
		{"38 14 * * 1-5", "2026-01-02 14:38:00", "2026-01-05 14:38:00"}, // Fri -> Mon
		{"5 4 * * 7", "2026-01-01 00:00:00", "2026-01-04 04:05:00"},     // dow 7 = Sunday
		{"0 0 13 * 5", "2026-01-01 00:00:00", "2026-01-02 00:00:00"},    // dom|dow: Friday the 2nd? no, Friday matches dow
		{"0 0 29 2 *", "2026-01-01 00:00:00", "2028-02-29 00:00:00"},    // leap day
		{"0 0 1 6 *", "2026-01-01 00:00:00", "2026-06-01 00:00:00"},
	}
	for _, c := range cases {
		s, err := parseSchedule(c.expr)
		if err != nil {
			t.Fatalf("parseSchedule(%q): %v", c.expr, err)
		}
		got := s.next(mustDate(t, c.from))
		if want := mustDate(t, c.want); !got.Equal(want) {
			t.Errorf("next(%q, %s) = %s, want %s", c.expr, c.from, got, c.want)
		}
	}
}

func TestScheduleNextDowOrDom(t *testing.T) {
	// Both day-of-month and weekday restricted: either may match (Vixie).
	s, err := parseSchedule("0 0 13 * 5")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-01-02 is a Friday (dow match) and 2026-01-13 is Tuesday the 13th.
	got := s.next(mustDate(t, "2026-01-01 00:00:00"))
	if want := mustDate(t, "2026-01-02 00:00:00"); !got.Equal(want) {
		t.Errorf("dow-only match: got %s, want %s", got, want)
	}
	got = s.next(mustDate(t, "2026-01-06 00:00:00"))
	if want := mustDate(t, "2026-01-09 00:00:00"); !got.Equal(want) { // next Friday
		t.Errorf("after Jan 6: got %s, want %s", got, want)
	}
}

// fakeClock is a controllable clock for deterministic scheduler tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestSchedulerFire(t *testing.T) {
	clk := &fakeClock{t: mustDate(t, "2026-01-01 10:00:00")}
	fired := make(chan Job, 1)
	s := New(Options{
		Now: clk.now,
		Run: func(_ context.Context, j Job) (string, error) {
			fired <- j
			return "", nil
		},
	})
	s.Add(Job{ID: "a", Name: "n", Schedule: "@every 1s", Prompt: "p", Enabled: true})

	s.tick(context.Background()) // arms the job, next at +1s
	clk.advance(time.Second)
	s.tick(context.Background()) // fires

	select {
	case j := <-fired:
		if j.ID != "a" || j.Prompt != "p" {
			t.Errorf("fired job = %+v", j)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job did not fire")
	}
	// Wait for the outcome to be recorded.
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap := s.Snapshot()
		if len(snap) == 1 && snap[0].LastRunAt != nil {
			if snap[0].LastStatus != "ok" || snap[0].LastError != "" {
				t.Errorf("state = %q / %q, want ok / empty", snap[0].LastStatus, snap[0].LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run state not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSchedulerRunErrorRecorded(t *testing.T) {
	clk := &fakeClock{t: mustDate(t, "2026-01-01 10:00:00")}
	s := New(Options{
		Now: clk.now,
		Run: func(_ context.Context, _ Job) (string, error) { return "", errors.New("boom") },
	})
	s.Add(Job{ID: "e", Schedule: "@every 1s", Enabled: true})

	s.tick(context.Background())
	clk.advance(time.Second)
	s.tick(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for {
		snap := s.Snapshot()
		if len(snap) == 1 && snap[0].LastRunAt != nil {
			if snap[0].LastStatus != "error" || snap[0].LastError != "boom" {
				t.Errorf("state = %q / %q, want error / boom", snap[0].LastStatus, snap[0].LastError)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("run state not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSchedulerSkipsWhileRunning(t *testing.T) {
	clk := &fakeClock{t: mustDate(t, "2026-01-01 10:00:00")}
	var mu sync.Mutex
	var calls int
	block := make(chan struct{})
	started := make(chan struct{}, 4)
	s := New(Options{
		Now: clk.now,
		Run: func(_ context.Context, _ Job) (string, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			started <- struct{}{}
			<-block // hold the job "running" across later ticks
			return "", nil
		},
	})
	s.Add(Job{ID: "b", Schedule: "@every 1s", Enabled: true})

	s.tick(context.Background())
	clk.advance(time.Second)
	s.tick(context.Background()) // first fire, now running
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not start")
	}

	clk.advance(2 * time.Second)
	s.tick(context.Background()) // due again but still running: occurrence dropped
	mu.Lock()
	if calls != 1 {
		t.Errorf("calls while running = %d, want 1 (skip, not queue)", calls)
	}
	mu.Unlock()

	close(block) // let the first run finish
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap := s.Snapshot()
		if snap[0].LastRunAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run state not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}

	clk.advance(time.Second)
	s.tick(context.Background()) // not running anymore: fires again
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("second run did not start")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("calls after release = %d, want 2", calls)
	}
}

func TestSchedulerDisabledAndBadJobs(t *testing.T) {
	clk := &fakeClock{t: mustDate(t, "2026-01-01 10:00:00")}
	var mu sync.Mutex
	var calls int
	s := New(Options{
		Now: clk.now,
		Run: func(_ context.Context, _ Job) (string, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return "", nil
		},
	})
	s.Add(
		Job{ID: "off", Schedule: "@every 1s", Enabled: false},
		Job{ID: "bad", Schedule: "not a schedule", Enabled: true},
		Job{ID: "good", Schedule: "@every 1s", Enabled: true},
	)
	if snap := s.Snapshot(); len(snap) != 2 {
		t.Fatalf("snapshot size = %d, want 2 (bad job dropped)", len(snap))
	}

	s.tick(context.Background())
	clk.advance(time.Second)
	s.tick(context.Background())
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (disabled job never fires)", calls)
	}
}

func TestSchedulerStartStop(t *testing.T) {
	done := make(chan struct{})
	var once sync.Once
	s := New(Options{
		Run: func(_ context.Context, _ Job) (string, error) {
			once.Do(func() { close(done) })
			return "", nil
		},
	})
	s.Add(Job{ID: "s", Schedule: "@every 1ms", Enabled: true})

	ctx := context.Background()
	s.Start(ctx)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("job never fired via Start")
	}
	s.Stop()
	s.Stop() // idempotent

	// A fresh loop after Stop still works.
	fired := make(chan struct{})
	var fireOnce sync.Once
	s2 := New(Options{
		Run: func(_ context.Context, _ Job) (string, error) {
			fireOnce.Do(func() { close(fired) })
			return "", nil
		},
	})
	s2.Add(Job{ID: "s2", Schedule: "@every 1ms", Enabled: true})
	s2.Start(ctx)
	defer s2.Stop()
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("job never fired after restart")
	}
}

// TestSchedulerRunPanicRecorded: a panicking job must not take the process
// down; the failure is recorded on the job state.
func TestSchedulerRunPanicRecorded(t *testing.T) {
	clk := &fakeClock{t: mustDate(t, "2026-01-01 10:00:00")}
	s := New(Options{Now: clk.now, Run: func(context.Context, Job) (string, error) { panic("boom") }})
	s.Add(Job{ID: "p", Schedule: "@every 1h", Prompt: "p", Enabled: true})
	s.tick(context.Background())
	clk.advance(time.Hour)
	s.tick(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	for {
		snap := s.Snapshot()
		if len(snap) == 1 && snap[0].LastStatus == "error" {
			if !strings.Contains(snap[0].LastError, "panic") {
				t.Errorf("LastError = %q", snap[0].LastError)
			}
			if snap[0].LastRunAt == nil {
				t.Error("LastRunAt not recorded")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("panic not recorded: %+v", snap)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSchedulerStopCancelsRunningJob: Stop cancels in-flight runs and waits.
func TestSchedulerStopCancelsRunningJob(t *testing.T) {
	started := make(chan struct{})
	s := New(Options{Run: func(ctx context.Context, _ Job) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}})
	s.Add(Job{ID: "long", Schedule: "@every 1ms", Prompt: "p", Enabled: true})
	s.Start(context.Background())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("job never started")
	}
	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop returned before the running job was cancelled")
	}
}

func TestSchedulerSpawnAfterStopDoesNotRun(t *testing.T) {
	calls := make(chan struct{}, 1)
	s := New(Options{Run: func(context.Context, Job) (string, error) {
		calls <- struct{}{}
		return "", nil
	}})
	s.Add(Job{ID: "job", Schedule: "@every 1h", Enabled: true})
	s.Stop()

	s.spawn(context.Background(), s.jobs["job"], map[string]bool{"job": true})

	select {
	case <-calls:
		t.Fatal("job ran after Stop")
	default:
	}
	if s.running["job"] {
		t.Fatal("rejected job left running state set")
	}
}

func TestSchedulerStopConcurrentSpawn(t *testing.T) {
	for i := 0; i < 100; i++ {
		s := New(Options{Run: func(context.Context, Job) (string, error) { return "", nil }})
		s.Add(Job{ID: "job", Schedule: "@every 1h", Enabled: true})
		start := make(chan struct{})
		var callers sync.WaitGroup
		for range 8 {
			callers.Add(1)
			go func() {
				defer callers.Done()
				<-start
				s.spawn(context.Background(), s.jobs["job"], map[string]bool{"job": true})
			}()
		}
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-start
			s.Stop()
		}()
		close(start)
		callers.Wait()
		s.Stop()
	}
}

func TestLoadMissingFile(t *testing.T) {
	jobs, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("Load(missing) error: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("Load(missing) = %d jobs, want 0", len(jobs))
	}
}

func TestLoadBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := os.WriteFile(path, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load(bad json) = nil error, want error")
	}
}

func TestLoadSkipsBadJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	data := `[{"id":"1","schedule":"@every 1s","prompt":"a","enabled":true},
	          {"id":"2","schedule":"bogus","prompt":"b","enabled":true},
	          {"id":"3","schedule":"0 0 * * *","prompt":"c","enabled":true}]`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	jobs, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("Load returned %d jobs, want 2 (bad one skipped)", len(jobs))
	}
	if jobs[0].ID != "1" || jobs[1].ID != "3" {
		t.Errorf("job IDs = %q, %q, want 1, 3", jobs[0].ID, jobs[1].ID)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	last := mustDate(t, "2026-01-01 10:00:00")
	jobs := []Job{
		{ID: "1", Name: "daily", Schedule: "38 14 * * 1-5", Prompt: "p1", Channel: "feishu:xxx", Enabled: true},
		{ID: "2", Schedule: "@every 30s", Prompt: "p2", Enabled: false,
			LastRunAt: &last, LastStatus: "error", LastError: "boom"},
	}
	if err := Save(path, jobs); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Load returned %d jobs, want 2", len(got))
	}
	if got[0].Channel != "feishu:xxx" || got[0].Schedule != "38 14 * * 1-5" {
		t.Errorf("job 0 round trip mismatch: %+v", got[0])
	}
	if got[1].LastStatus != "error" || got[1].LastError != "boom" ||
		got[1].LastRunAt == nil || !got[1].LastRunAt.Equal(last) {
		t.Errorf("job 1 state lost: %+v", got[1])
	}
}

func TestSaveError(t *testing.T) {
	err := Save(filepath.Join(t.TempDir(), "no-such-dir", "jobs.json"), []Job{{ID: "x", Schedule: "@every 1s"}})
	if err == nil {
		t.Fatal("Save into missing dir = nil error, want error")
	}
}

func TestSaveEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := Save(path, nil); err != nil {
		t.Fatalf("Save(nil): %v", err)
	}
	jobs, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("Load after Save(nil) = %d jobs, want 0", len(jobs))
	}
}

// TestSchedulerAfterChain: a successful upstream run fires downstream jobs
// immediately (event chain, no polling); failures do not.
func TestSchedulerAfterChain(t *testing.T) {
	clk := &fakeClock{t: mustDate(t, "2026-01-01 10:00:00")}
	done := make(chan Job, 2)
	var upErr error
	s := New(Options{
		Now: clk.now,
		Run: func(_ context.Context, j Job) (string, error) {
			if j.ID == "up" {
				return "upstream report", upErr
			}
			done <- j
			return "", nil
		},
	})
	s.Add(Job{ID: "up", Schedule: "@every 1h", Prompt: "p", Enabled: true})
	s.Add(Job{ID: "down", Prompt: "q", Enabled: true, After: []string{"up"}, ContextFrom: []string{"up"}})

	s.tick(context.Background())
	clk.advance(time.Hour)
	s.tick(context.Background())
	select {
	case j := <-done:
		if !strings.Contains(j.Prompt, "upstream report") {
			t.Errorf("context_from not injected into prompt: %q", j.Prompt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("downstream never fired after upstream success")
	}

	// Upstream failure must not fire downstream.
	upErr = errors.New("boom")
	clk.advance(time.Hour)
	s.tick(context.Background())
	select {
	case j := <-done:
		t.Errorf("downstream fired after failed upstream: %+v", j)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSchedulerAfterCycleGuard: A→B→A chains run each job once per
// propagation instead of ping-ponging forever.
func TestSchedulerAfterCycleGuard(t *testing.T) {
	clk := &fakeClock{t: mustDate(t, "2026-01-01 10:00:00")}
	var mu sync.Mutex
	calls := map[string]int{}
	done := make(chan struct{}, 8)
	s := New(Options{
		Now: clk.now,
		Run: func(_ context.Context, j Job) (string, error) {
			mu.Lock()
			calls[j.ID]++
			mu.Unlock()
			done <- struct{}{}
			return "", nil
		},
	})
	s.Add(Job{ID: "a", Schedule: "@every 1h", Prompt: "p", Enabled: true, After: []string{"b"}})
	s.Add(Job{ID: "b", Prompt: "q", Enabled: true, After: []string{"a"}})

	s.tick(context.Background()) // first occurrence not due yet
	clk.advance(time.Hour)
	s.tick(context.Background()) // fires a → chain {a}
	// Wait for both chain firings to settle.
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("chain stopped early, calls=%v", calls)
		}
	}
	time.Sleep(200 * time.Millisecond) // any runaway would still be firing
	mu.Lock()
	defer mu.Unlock()
	if calls["a"] != 1 || calls["b"] != 1 {
		t.Errorf("cycle ran a=%d b=%d, want 1 each", calls["a"], calls["b"])
	}
}
