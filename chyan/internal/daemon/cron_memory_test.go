package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/cron"
	"chyan.tech/chyan/internal/store"
)

// cronStubScheduler returns a scheduler whose runs do nothing but succeed.
func cronStubScheduler(run func(context.Context, cron.Job) (string, error)) *cron.Scheduler {
	return cron.New(cron.Options{Run: run})
}

func createCronViaAPI(t *testing.T, s *Server, req api.CronJobRequest) cron.Job {
	t.Helper()
	w, r := doJSON(t, http.MethodPost, "/api/v1/cron", req, "test-token")
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create cron = %d %s", w.Code, w.Body.String())
	}
	var job cron.Job
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestCronFileOnlyManagement(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	// Seed jobs.json directly (CLI/import shape).
	path := filepath.Join(s.app.Home(), "jobs.json")
	seed := []cron.Job{{ID: "seed", Name: "夜间整理", Schedule: "0 3 * * *", Prompt: "整理", Enabled: true}}
	if err := cron.Save(path, seed); err != nil {
		t.Fatal(err)
	}
	w, r := doJSON(t, http.MethodGet, "/api/v1/cron", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || len(w.Body.String()) == 0 {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	var jobs []cron.Job
	if err := json.Unmarshal(w.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "seed" {
		t.Fatalf("jobs = %+v", jobs)
	}

	created := createCronViaAPI(t, s, api.CronJobRequest{
		Name: "日报", Schedule: "@every 1h", Prompt: "写日报", Enabled: true,
	})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("jobs.json invalid: %s", data)
	}

	w, r = doJSON(t, http.MethodPatch, "/api/v1/cron/"+created.ID,
		api.CronJobRequest{Name: "日报2", Schedule: "@every 2h", Prompt: "写日报", Enabled: false}, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d %s", w.Code, w.Body.String())
	}
	var updated cron.Job
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Name != "日报2" || updated.Enabled {
		t.Errorf("updated = %+v", updated)
	}

	w, r = doJSON(t, http.MethodDelete, "/api/v1/cron/"+created.ID, nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}

	// Without a scheduler an immediate run is not possible.
	w, r = doJSON(t, http.MethodPost, "/api/v1/cron/seed/run", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("run without scheduler = %d", w.Code)
	}

	w, r = doJSON(t, http.MethodPatch, "/api/v1/cron/missing",
		api.CronJobRequest{Name: "x", Schedule: "@every 1h", Prompt: "x"}, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("update missing = %d", w.Code)
	}
	w, r = doJSON(t, http.MethodDelete, "/api/v1/cron/missing", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete missing = %d", w.Code)
	}
}

func TestCronValidation(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	cases := []struct {
		name string
		req  api.CronJobRequest
	}{
		{"missing name", api.CronJobRequest{Schedule: "@every 1h", Prompt: "p"}},
		{"bad schedule", api.CronJobRequest{Name: "n", Schedule: "not a cron", Prompt: "p"}},
		{"missing schedule", api.CronJobRequest{Name: "n", Prompt: "p"}},
		{"empty body", api.CronJobRequest{Name: "n", Schedule: "@every 1h"}},
		{"bad workdir", api.CronJobRequest{Name: "n", Schedule: "@every 1h", Prompt: "p",
			WorkDir: filepath.Join(t.TempDir(), "missing")}},
	}
	for _, tc := range cases {
		w, r := doJSON(t, http.MethodPost, "/api/v1/cron", tc.req, "test-token")
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s", tc.name, w.Code, w.Body.String())
		}
	}
}

func TestCronSchedulerRunNowAndPersist(t *testing.T) {
	s, _, _ := newTestServer(t)
	ran := make(chan string, 1)
	var sched *cron.Scheduler
	sched = cronStubScheduler(func(_ context.Context, j cron.Job) (string, error) {
		sched.SetLastSession(j.ID, "ses-cron")
		ran <- j.ID
		return "ok", nil
	})
	s.SetScheduler(sched)
	h := s.Handler()

	job := createCronViaAPI(t, s, api.CronJobRequest{
		Name: "巡检", Schedule: "@every 1h", Prompt: "检查", Enabled: true,
	})

	w, r := doJSON(t, http.MethodPost, "/api/v1/cron/"+job.ID+"/run", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("run = %d %s", w.Code, w.Body.String())
	}
	select {
	case id := <-ran:
		if id != job.ID {
			t.Errorf("ran %q, want %q", id, job.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job did not run")
	}

	// Poll until the run is recorded, then check the session linkage.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w, r = doJSON(t, http.MethodGet, "/api/v1/cron", nil, "test-token")
		h.ServeHTTP(w, r)
		var jobs []cron.Job
		if json.Unmarshal(w.Body.Bytes(), &jobs) == nil && len(jobs) == 1 && jobs[0].LastStatus == "ok" {
			if jobs[0].LastSessionID != "ses-cron" {
				t.Errorf("last session = %q", jobs[0].LastSessionID)
			}
			if jobs[0].LastRunAt == nil {
				t.Error("last run time not recorded")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run state was not recorded")

	// Disabled jobs refuse an immediate run.
	w, r = doJSON(t, http.MethodPatch, "/api/v1/cron/"+job.ID,
		api.CronJobRequest{Name: "巡检", Schedule: "@every 1h", Prompt: "检查", Enabled: false}, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", w.Code, w.Body.String())
	}
	w, r = doJSON(t, http.MethodPost, "/api/v1/cron/"+job.ID+"/run", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("disabled run = %d %s", w.Code, w.Body.String())
	}

	// Unknown job ids are 404.
	w, r = doJSON(t, http.MethodPost, "/api/v1/cron/missing/run", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing run = %d", w.Code)
	}
}

func TestMemoryEndpoints(t *testing.T) {
	s, a, _ := newTestServer(t)
	h := s.Handler()
	ctx := context.Background()
	m, err := a.Memory().AddMemory(ctx, store.Memory{
		Kind: "fact", Name: "部署目录", Keywords: []string{"deploy"},
		Content: "prod deploy dir is /srv/app",
	})
	if err != nil {
		t.Fatal(err)
	}

	w, r := doJSON(t, http.MethodGet, "/api/v1/memory", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	var entries []api.MemoryEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != m.ID || entries[0].Status != "active" {
		t.Fatalf("entries = %+v", entries)
	}

	w, r = doJSON(t, http.MethodGet, "/api/v1/memory?q=deploy&domain=global", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || len(w.Body.String()) == 0 {
		t.Fatalf("search = %d %s", w.Code, w.Body.String())
	}

	// Project filtering rejects an unknown directory.
	w, r = doJSON(t, http.MethodGet,
		"/api/v1/memory?project="+filepath.Join(t.TempDir(), "missing"), nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad project = %d", w.Code)
	}

	w, r = doJSON(t, http.MethodPost, "/api/v1/memory/"+m.ID+"/supersede", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("supersede = %d %s", w.Code, w.Body.String())
	}
	var sup api.MemoryEntry
	if err := json.Unmarshal(w.Body.Bytes(), &sup); err != nil {
		t.Fatal(err)
	}
	if sup.Status != "superseded" {
		t.Errorf("supersede status = %q", sup.Status)
	}

	w, r = doJSON(t, http.MethodPost, "/api/v1/memory/missing/supersede", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing supersede = %d %s", w.Code, w.Body.String())
	}
	w, r = doJSON(t, http.MethodPost, "/api/v1/memory/bad%2Fid/supersede", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad id supersede = %d", w.Code)
	}
}

func TestDreamEndpoints(t *testing.T) {
	s, a, _ := newTestServer(t)
	h := s.Handler()

	w, r := doJSON(t, http.MethodGet, "/api/v1/memory/dream", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("dream before run = %d", w.Code)
	}

	w, r = doJSON(t, http.MethodPost, "/api/v1/memory/dream", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("dream run = %d %s", w.Code, w.Body.String())
	}
	reportPath := filepath.Join(a.Home(), "state", "dream-report.json")
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatalf("dream report not persisted: %v", err)
	}

	w, r = doJSON(t, http.MethodGet, "/api/v1/memory/dream", nil, "test-token")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || len(w.Body.String()) == 0 {
		t.Fatalf("dream get = %d %s", w.Code, w.Body.String())
	}
}
