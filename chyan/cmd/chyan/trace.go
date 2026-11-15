// trace.go — read local agent spans by trace, session or task id (FR-13).
// Text output is a human summary; --json keeps the full span rows for scripts.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"chyan.tech/chyan/internal/store"
	"chyan.tech/chyan/internal/trace"
)

// spanLister is the persistence capability the trace viewer needs. Keeping
// it off store.Store means test doubles never grow a method they do not use.
type spanLister interface {
	ListSpans(ctx context.Context, q store.SpanQuery) ([]trace.Span, error)
}

func traceCmd() *cobra.Command {
	var (
		home   string
		asJSON bool
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "trace <trace-id|session-id|task-id>",
		Short: "Show local agent spans and slow/expensive attribution (FR-13)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := homeFlag(home)
			if err != nil {
				return err
			}
			st, err := openDataStore(h)
			if err != nil {
				return err
			}
			defer st.Close()
			lister, ok := st.(spanLister)
			if !ok {
				return fmt.Errorf("trace: this store does not support span queries")
			}
			spans, err := resolveSpans(cmd.Context(), lister, args[0], limit)
			if err != nil {
				return err
			}
			if len(spans) == 0 {
				return fmt.Errorf("trace: no spans for %q (traces are recorded per turn; check the id)", args[0])
			}
			summary := trace.Summarize(spans)
			if asJSON {
				out := struct {
					Summary trace.Summary `json:"summary"`
					Spans   []trace.Span  `json:"spans"`
				}{summary, spans}
				b, err := json.MarshalIndent(out, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(b))
				return nil
			}
			printTrace(summary, spans)
			return nil
		},
	}
	cmd.Flags().StringVar(&home, "home", "", "chyan data root (default ~/.chyan)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the raw summary and spans as JSON")
	cmd.Flags().IntVar(&limit, "limit", 1000, "maximum number of spans to load")
	return cmd
}

// resolveSpans tries the selector as a trace id first, then a session id, then
// a task id: the CLI takes one positional value and users rarely remember
// which kind of id they copied.
func resolveSpans(ctx context.Context, l spanLister, id string, limit int) ([]trace.Span, error) {
	for _, q := range []store.SpanQuery{
		{TraceID: id, Limit: limit},
		{SessionID: id, Limit: limit},
		{TaskID: id, Limit: limit},
	} {
		spans, err := l.ListSpans(ctx, q)
		if err != nil {
			return nil, err
		}
		if len(spans) > 0 {
			return spans, nil
		}
	}
	return nil, nil
}

func printTrace(s trace.Summary, spans []trace.Span) {
	fmt.Printf("trace %s\n", s.TraceID)
	fmt.Printf("窗口：%s → %s（%s，%d spans）\n",
		s.Start.Format("15:04:05.000"), s.End.Format("15:04:05.000"), ms(s.DurationMS), s.SpanCount)
	fmt.Printf("耗时：模型 %s｜工具 %s｜等人工 %s｜记忆 %s｜子 Agent %s｜编排 %s\n",
		ms(s.ModelMS), ms(s.ToolMS), ms(s.WaitMS), ms(s.MemoryMS), ms(s.SubagentMS), ms(s.OverheadMS))
	fmt.Printf("用量：in %d｜out %d｜cache read %d｜尝试 %d｜成本 $%.4f\n",
		s.TokensIn, s.TokensOut, s.CacheRead, s.Attempts, s.CostUSD)
	if len(s.Errors) > 0 {
		fmt.Printf("错误：%s\n", formatErrors(s.Errors))
	}
	fmt.Println("spans：")
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].Start.Before(spans[j].Start) })
	for _, sp := range spans {
		line := fmt.Sprintf("  %-18s %-16s %s", sp.Kind, sp.Name, ms(sp.Duration().Milliseconds()))
		if sp.AgentID != "" {
			line += " agent=" + shortID(sp.AgentID)
		}
		if sp.Status != trace.StatusOK {
			line += " " + string(sp.Status)
		}
		if sp.ErrorClass != "" {
			line += " error=" + sp.ErrorClass
		}
		if len(sp.Attrs) > 0 {
			line += " " + formatAttrs(sp.Attrs)
		}
		fmt.Println(line)
	}
}

func formatErrors(errs map[string]int) string {
	keys := make([]string, 0, len(errs))
	for k := range errs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, errs[k]))
	}
	return strings.Join(parts, ", ")
}

func formatAttrs(attrs map[string]any) string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, attrs[k]))
	}
	return strings.Join(parts, " ")
}

func ms(v int64) string {
	switch {
	case v < 1000:
		return fmt.Sprintf("%dms", v)
	case v < 60000:
		return fmt.Sprintf("%.1fs", float64(v)/1000)
	default:
		return fmt.Sprintf("%dm%02ds", v/60000, (v%60000)/1000)
	}
}

func shortID(id string) string {
	if len(id) <= 10 {
		return id
	}
	return id[:10]
}
