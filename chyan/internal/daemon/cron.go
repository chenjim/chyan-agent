// Cron management handlers: the Web automation view lists, creates, edits,
// toggles, deletes and runs scheduled jobs (FR-12 W1). jobs.json stays the
// truth source; the scheduler, when attached, owns scheduling and runtime
// state and serializes the writes.
package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/cron"
)

// cronJobsPath is the job file the CLI and daemon share.
func (s *Server) cronJobsPath() string {
	return filepath.Join(s.app.Home(), "jobs.json")
}

// cronJobs returns the current jobs sorted by name for a stable Web list.
func (s *Server) cronJobs(ctx context.Context) ([]cron.Job, error) {
	var jobs []cron.Job
	var err error
	if s.cronSched != nil {
		jobs = s.cronSched.Snapshot()
	} else {
		jobs, err = cron.Load(s.cronJobsPath())
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Name != jobs[j].Name {
			return jobs[i].Name < jobs[j].Name
		}
		return jobs[i].ID < jobs[j].ID
	})
	return jobs, err
}

// findCronJob returns one job by id.
func (s *Server) findCronJob(ctx context.Context, id string) (cron.Job, bool) {
	jobs, err := s.cronJobs(ctx)
	if err != nil {
		return cron.Job{}, false
	}
	for _, j := range jobs {
		if j.ID == id {
			return j, true
		}
	}
	return cron.Job{}, false
}

// handleListCron lists scheduled jobs (GET /api/v1/cron), including last
// run/status/session so the view can link to the transcript.
func (s *Server) handleListCron(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.cronJobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

// handleCreateCron adds one job (POST /api/v1/cron).
func (s *Server) handleCreateCron(w http.ResponseWriter, r *http.Request) {
	var req api.CronJobRequest
	if err := decodeBodyLimit(w, r, &req, maxTaskBodyBytes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.cronJobFromRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := newCronID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	job.ID = id
	if s.cronSched != nil {
		s.cronSched.Upsert(job)
		if err := s.cronSched.Save(s.cronJobsPath()); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		jobs, err := s.cronJobs(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		jobs = append(jobs, job)
		if err := cron.Save(s.cronJobsPath(), jobs); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusCreated, job)
}

// handleUpdateCron edits one job definition (PATCH /api/v1/cron/{id}).
// Runtime state (last run/status/session) survives the edit.
func (s *Server) handleUpdateCron(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old, ok := s.findCronJob(r.Context(), id)
	if !ok {
		writeError(w, http.StatusNotFound, "cron job not found")
		return
	}
	var req api.CronJobRequest
	if err := decodeBodyLimit(w, r, &req, maxTaskBodyBytes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.cronJobFromRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job.ID = id
	if s.cronSched != nil {
		s.cronSched.Upsert(job) // Upsert keeps the old runtime fields
		if err := s.cronSched.Save(s.cronJobsPath()); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		job.LastRunAt, job.LastStatus = old.LastRunAt, old.LastStatus
		job.LastError, job.LastSessionID = old.LastError, old.LastSessionID
		jobs, err := s.cronJobs(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for i := range jobs {
			if jobs[i].ID == id {
				jobs[i] = job
			}
		}
		if err := cron.Save(s.cronJobsPath(), jobs); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	updated, ok := s.findCronJob(r.Context(), id)
	if !ok {
		updated = job
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteCron removes one job (DELETE /api/v1/cron/{id}).
func (s *Server) handleDeleteCron(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findCronJob(r.Context(), id); !ok {
		writeError(w, http.StatusNotFound, "cron job not found")
		return
	}
	if s.cronSched != nil {
		s.cronSched.Remove(id)
		if err := s.cronSched.Save(s.cronJobsPath()); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		jobs, err := s.cronJobs(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		kept := jobs[:0]
		for _, j := range jobs {
			if j.ID != id {
				kept = append(kept, j)
			}
		}
		if err := cron.Save(s.cronJobsPath(), kept); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRunCron fires one job immediately (POST /api/v1/cron/{id}/run).
// The job's schedule is untouched; a running occurrence is never duplicated.
func (s *Server) handleRunCron(w http.ResponseWriter, r *http.Request) {
	if s.cronSched == nil {
		writeError(w, http.StatusNotImplemented, "cron scheduler is not running")
		return
	}
	// Run under the daemon lifetime, not the HTTP request: the handler returns
	// immediately while the job keeps running (and cancels on shutdown).
	started, err := s.cronSched.RunNow(s.daemonContext(), r.PathValue("id"))
	switch {
	case errors.Is(err, cron.ErrJobNotFound):
		writeError(w, http.StatusNotFound, "cron job not found")
		return
	case errors.Is(err, cron.ErrJobDisabled):
		writeError(w, http.StatusConflict, "cron job is disabled")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !started {
		writeError(w, http.StatusConflict, "cron job is already running")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

// cronJobFromRequest validates one editable job definition.
func (s *Server) cronJobFromRequest(req api.CronJobRequest) (cron.Job, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return cron.Job{}, errors.New("name is required")
	}
	if len([]rune(name)) > 200 {
		return cron.Job{}, errors.New("name is too long (max 200 characters)")
	}
	if strings.TrimSpace(req.Schedule) == "" && len(req.After) == 0 {
		return cron.Job{}, errors.New("schedule is required (or set after)")
	}
	if req.Schedule != "" {
		if err := cron.ValidateSchedule(req.Schedule); err != nil {
			return cron.Job{}, err
		}
	}
	workDir := req.WorkDir
	if workDir != "" {
		resolved, err := app.ResolveWorkDir(s.app.WorkDir(), workDir)
		if err != nil {
			return cron.Job{}, err
		}
		workDir = resolved
	}
	if req.Prompt == "" && req.Script == "" {
		return cron.Job{}, errors.New("prompt or script is required")
	}
	return cron.Job{
		Name: name, Schedule: strings.TrimSpace(req.Schedule),
		Prompt: req.Prompt, Channel: strings.TrimSpace(req.Channel),
		Enabled: req.Enabled, WorkDir: workDir,
		Script: req.Script, NoAgent: req.NoAgent,
		After: req.After, ContextFrom: req.ContextFrom,
	}, nil
}

// newCronID generates a stable job identifier ("job-<16 hex>").
func newCronID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("job-%016x", buf), nil
}
