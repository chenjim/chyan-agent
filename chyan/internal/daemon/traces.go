// traces.go — read-only span API for the Web trace viewer (FR-13/U4).
// The CLI (`chyan trace`) reads the same rows straight from the store; this
// endpoint exposes the identical attribution over HTTP so the workbench can
// expand a turn without a terminal.
package daemon

import (
	"context"
	"net/http"
	"sort"
	"strconv"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/store"
	"chyan.tech/chyan/internal/trace"
)

// spanLister is the persistence capability the endpoint needs (see cmd/chyan).
type spanLister interface {
	ListSpans(ctx context.Context, q store.SpanQuery) ([]trace.Span, error)
}

// traceLimit caps one response: a long session has one trace per turn and
// every turn has ~10 spans, so 2000 covers 100+ turns without a pagination
// protocol the viewer would have to implement.
const traceLimit = 2000

// handleTraces returns one session/trace/task's spans grouped per turn
// (GET /api/v1/traces?session_id=...&trace_id=...&task_id=...&limit=...).
// Turns are newest first; spans inside a turn keep start order.
func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	q := store.SpanQuery{
		SessionID: r.URL.Query().Get("session_id"),
		TraceID:   r.URL.Query().Get("trace_id"),
		TaskID:    r.URL.Query().Get("task_id"),
		// Newest first so the limit sheds the oldest turns: the viewer always
		// shows the latest activity of a long session (groupTurns re-sorts each
		// turn chronologically afterwards).
		Newest: true,
	}
	if q.SessionID == "" && q.TraceID == "" && q.TaskID == "" {
		writeError(w, http.StatusBadRequest, "traces need session_id, trace_id or task_id")
		return
	}
	q.Limit = traceLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		q.Limit = min(n, traceLimit)
	}
	lister, ok := s.store.(spanLister)
	if !ok {
		// Tracing disabled for this store (in-memory test stores): an empty
		// answer is the truth, not an error.
		writeJSON(w, http.StatusOK, api.TraceResponse{Turns: []api.TraceTurn{}})
		return
	}
	spans, err := lister.ListSpans(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.TraceResponse{Turns: groupTurns(spans)})
}

// groupTurns buckets spans by trace id and summarizes each bucket, newest
// turn first. Summaries come from trace.Summarize so the numbers match
// `chyan trace` exactly (no second implementation to drift).
func groupTurns(spans []trace.Span) []api.TraceTurn {
	byTrace := make(map[string][]trace.Span)
	order := make([]string, 0, len(spans))
	for _, sp := range spans {
		if _, seen := byTrace[sp.TraceID]; !seen {
			order = append(order, sp.TraceID)
		}
		byTrace[sp.TraceID] = append(byTrace[sp.TraceID], sp)
	}
	turns := make([]api.TraceTurn, 0, len(order))
	for _, id := range order {
		group := byTrace[id]
		sortSpans(group)
		turns = append(turns, api.TraceTurn{Summary: trace.Summarize(group), Spans: group})
	}
	sort.SliceStable(turns, func(i, j int) bool {
		return turns[i].Summary.Start.After(turns[j].Summary.Start)
	})
	if turns == nil {
		return []api.TraceTurn{}
	}
	return turns
}

// sortSpans orders one turn's spans by start time (stable on id) so the
// viewer renders the tree in the order things actually happened.
func sortSpans(spans []trace.Span) {
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].Start.Equal(spans[j].Start) {
			return spans[i].ID < spans[j].ID
		}
		return spans[i].Start.Before(spans[j].Start)
	})
}
