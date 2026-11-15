// Permission engine wiring: config rules -> core.PermSet -> core.ReviewGate.
package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"chyan.tech/chyan/internal/config"
	"chyan.tech/chyan/internal/core"
)

// buildChecker maps the config rule list onto the core rule engine. Entries
// with an invalid effect are dropped by NewPermSet: a malformed entry can
// never widen permissions.
func buildChecker(cfg config.Config) *core.PermSet {
	rules := make([]core.Rule, 0, len(cfg.Permission))
	for _, p := range cfg.Permission {
		rules = append(rules, core.Rule{
			Effect:   core.Effect(p.Effect),
			Action:   p.Action,
			Resource: p.Resource,
		})
	}
	return core.NewPermSet(rules...)
}

// reviewMode parses the configured review mode; unknown values fall back to
// ReviewAI (the appendix A default). --auto forces ReviewFull.
func reviewMode(cfg config.Config, auto bool) core.ReviewMode {
	if auto {
		return core.ReviewFull
	}
	switch cfg.Review.Mode {
	case "human":
		return core.ReviewHuman
	case "full":
		return core.ReviewFull
	default:
		return core.ReviewAI
	}
}

// buildGate assembles the review gate: rule checker + optional LLM risk
// reviewer + human approver channel. [review].model picks the reviewer model;
// when unset the main model is used with reasoning disabled (cheap one-word
// verdicts; the rule heuristic already handles the obvious cases).
func buildGate(cfg config.Config, opts Options) (ToolGate, error) {
	reviewer := opts.Reviewer
	if reviewer == nil {
		model := cfg.Review.Model
		if model == "" {
			model = cfg.LLM.Model
		}
		if model != "" {
			if opts.Backend == nil {
				return nil, fmt.Errorf("app: review model %q needs an LLM backend", model)
			}
			reviewer = &llmReviewer{backend: opts.Backend, model: model,
				reasoningEffort: "none"}
		}
	}
	g := core.NewReviewGate(reviewMode(cfg, opts.Auto), reviewer, opts.Approver, cfg.Review.EscalateTimeout.D()).
		WithChecker(buildChecker(cfg))
	return &reviewToolGate{gate: g}, nil
}

// reviewToolGate adapts core.ReviewGate to the ToolGate interface.
type reviewToolGate struct{ gate *core.ReviewGate }

// Ask implements ToolGate: the verdict string feeds the audit trail.
func (g *reviewToolGate) Ask(ctx context.Context, req core.ApprovalRequest) (bool, string, error) {
	d, err := g.gate.DecideRequest(ctx, req)
	if err != nil {
		return false, "", err
	}
	return d.Approved, string(d.Verdict), nil
}

// llmReviewer is the D23 LLM fallback: a one-shot risk classification over
// the app backend. Parse failure degrades to uncertain, never to allow.
type llmReviewer struct {
	backend core.LLMBackend
	model   string
	// reasoningEffort caps thinking on reasoning models ("none" = off);
	// a one-word verdict never pays for reasoning tokens.
	reasoningEffort string
}

// Review implements core.RiskReviewer: judge risk as low|high|uncertain.
func (r *llmReviewer) Review(ctx context.Context, action, resource, detail string) (core.RiskVerdict, error) {
	prompt := fmt.Sprintf("Judge risk of this agent tool call. Answer with one word: low|high|uncertain.\naction: %s\nresource: %s\ndetail: %s",
		action, resource, detail)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := r.backend.Chat(ctx, core.Request{
		Model:           r.model,
		ReasoningEffort: r.reasoningEffort,
		Messages:        []core.Message{{Role: core.RoleUser, Content: []core.Part{{Type: "text", Text: prompt}}}},
	})
	if err != nil {
		return core.RiskUncertain, err
	}
	var text strings.Builder
	for _, p := range resp.Message.Content {
		text.WriteString(p.Text)
	}
	return parseRiskVerdict(text.String()), nil
}

// parseRiskVerdict reads the first word of the reply; anything unrecognized
// stays uncertain.
func parseRiskVerdict(s string) core.RiskVerdict {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return core.RiskUncertain
	}
	switch strings.ToLower(strings.Trim(fields[0], ".,;:!?")) {
	case "low":
		return core.RiskLow
	case "high":
		return core.RiskHigh
	default:
		return core.RiskUncertain
	}
}
