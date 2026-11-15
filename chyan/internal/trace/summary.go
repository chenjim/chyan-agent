// Summary turns a span tree into the four numbers a slow/expensive turn must
// answer: model time, tool execution time, human-approval wait, and orchestration
// overhead. Waiting for approval is never counted as tool execution (FR-13).
package trace

import (
	"sort"
	"time"
)

// Summary is the aggregated attribution of one trace.
type Summary struct {
	TraceID    string         `json:"trace_id"`
	SpanCount  int            `json:"span_count"`
	Start      time.Time      `json:"start"`
	End        time.Time      `json:"end"`
	DurationMS int64          `json:"duration_ms"`
	ModelMS    int64          `json:"model_ms"`
	ToolMS     int64          `json:"tool_execution_ms"`
	WaitMS     int64          `json:"approval_wait_ms"`
	MemoryMS   int64          `json:"memory_ms"`
	SubagentMS int64          `json:"subagent_ms"`
	OverheadMS int64          `json:"overhead_ms"`
	TokensIn   int            `json:"tokens_in"`
	TokensOut  int            `json:"tokens_out"`
	CacheRead  int            `json:"cache_read_tokens"`
	CostUSD    float64        `json:"cost_usd"`
	Attempts   int            `json:"attempts"`
	Errors     map[string]int `json:"errors,omitempty"`
}

// Summarize aggregates spans belonging to one or more traces. Spans are not
// required to be sorted; the earliest start and latest end define the window.
func Summarize(spans []Span) Summary {
	s := Summary{Errors: map[string]int{}}
	if len(spans) == 0 {
		return s
	}
	// Durations are united per kind, not summed: parallel sub-agent work
	// overlaps, and summing it would report more model/tool time than wall
	// clock and push orchestration overhead to zero.
	var model, tool, wait, mem, sub []timeInterval
	for _, sp := range spans {
		iv := timeInterval{start: sp.Start, end: sp.End}
		switch sp.Kind {
		case KindToolWaitApproval:
			wait = append(wait, iv)
		case KindToolExecution:
			tool = append(tool, iv)
		case KindLLM:
			model = append(model, iv)
			s.TokensIn += attrInt(sp.Attrs, "tokens_in")
			s.TokensOut += attrInt(sp.Attrs, "tokens_out")
			s.CacheRead += attrInt(sp.Attrs, "cache_read_tokens")
			s.CostUSD += attrFloat(sp.Attrs, "cost_usd")
			if a := attrInt(sp.Attrs, "attempts"); a > 0 {
				s.Attempts += a
			}
		case KindMemoryRecall, KindMemoryWrite:
			mem = append(mem, iv)
		case KindSubagent:
			sub = append(sub, iv)
		}
		if sp.ErrorClass != "" && sp.Status == StatusError {
			s.Errors[sp.ErrorClass]++
		}
	}
	s.ModelMS = unionMS(model)
	s.WaitMS = unionMS(wait)
	// Tool spans include the approval wait that happens inside Execute.
	s.ToolMS = subtractMS(tool, wait)
	s.MemoryMS = unionMS(mem)
	s.SubagentMS = unionMS(sub)
	s.TraceID = rootTraceID(spans)
	s.SpanCount = len(spans)
	// Window = earliest start to latest end. Iterating beats sorting: a root
	// span starts first but ends last, so "first.last" would truncate the turn.
	s.Start = spans[0].Start
	s.End = spans[0].End
	for _, sp := range spans[1:] {
		if sp.Start.Before(s.Start) {
			s.Start = sp.Start
		}
		if sp.End.After(s.End) {
			s.End = sp.End
		}
	}
	s.DurationMS = s.End.Sub(s.Start).Milliseconds()
	s.OverheadMS = s.DurationMS - s.ModelMS - s.ToolMS - s.WaitMS - s.MemoryMS
	if s.OverheadMS < 0 {
		s.OverheadMS = 0
	}
	return s
}

// rootTraceID prefers the trace of the earliest root turn.
func rootTraceID(spans []Span) string {
	for _, sp := range spans {
		if sp.Kind == KindTurn && sp.ParentID == "" {
			return sp.TraceID
		}
	}
	if len(spans) > 0 {
		return spans[0].TraceID
	}
	return ""
}

// timeInterval is one span's wall-clock occupation.
type timeInterval struct {
	start time.Time
	end   time.Time
}

// unionMS returns the wall-clock duration covered by at least one interval.
func unionMS(in []timeInterval) int64 {
	var total time.Duration
	for _, iv := range mergeIntervals(in) {
		total += iv.end.Sub(iv.start)
	}
	return total.Milliseconds()
}

// mergeIntervals normalizes intervals, dropping empty/negative ones and
// merging overlaps so concurrent work is counted once.
func mergeIntervals(in []timeInterval) []timeInterval {
	valid := make([]timeInterval, 0, len(in))
	for _, iv := range in {
		if iv.end.After(iv.start) {
			valid = append(valid, iv)
		}
	}
	if len(valid) == 0 {
		return nil
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].start.Before(valid[j].start) })
	out := []timeInterval{valid[0]}
	for _, iv := range valid[1:] {
		last := &out[len(out)-1]
		if !iv.start.After(last.end) {
			if iv.end.After(last.end) {
				last.end = iv.end
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// subtractMS returns the duration covered by base but not by cut.
func subtractMS(base, cut []timeInterval) int64 {
	if len(cut) == 0 {
		return unionMS(base)
	}
	ms := unionMS(base) - overlapMS(base, cut)
	if ms < 0 {
		return 0
	}
	return ms
}

// overlapMS returns the duration covered by both interval sets.
func overlapMS(a, b []timeInterval) int64 {
	ma, mb := mergeIntervals(a), mergeIntervals(b)
	var total time.Duration
	i, j := 0, 0
	for i < len(ma) && j < len(mb) {
		start := ma[i].start
		if mb[j].start.After(start) {
			start = mb[j].start
		}
		end := ma[i].end
		if mb[j].end.Before(end) {
			end = mb[j].end
		}
		if end.After(start) {
			total += end.Sub(start)
		}
		if ma[i].end.Before(mb[j].end) {
			i++
		} else {
			j++
		}
	}
	return total.Milliseconds()
}

func attrInt(attrs map[string]any, key string) int {
	if attrs == nil {
		return 0
	}
	switch v := attrs[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func attrFloat(attrs map[string]any, key string) float64 {
	if attrs == nil {
		return 0
	}
	switch v := attrs[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}
