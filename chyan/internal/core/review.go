// Review gate deciding ask-effect requests per review mode (architecture 4.2,
// D10/D23/D24): heuristic-first + LLM fallback in ai mode, human push with
// escalation timeout in human mode, auto-approve in full mode. Explicit deny
// stays a hard block in every mode.
package core

import (
	"context"
	"errors"
	"strings"
	"time"
)

// DefaultEscalateTimeout is how long a human has to answer an escalated ask.
// Running out of time denies the request and stops the run (D24).
const DefaultEscalateTimeout = time.Minute

// ErrApprovalTimeout marks an ask whose human decision timed out. It is a
// fatal tool error: the run stops instead of silently continuing.
var ErrApprovalTimeout = errors.New("core: approval timed out")

// GateDecision is one review outcome, carrying the reason for the audit trail.
type GateDecision struct {
	Approved bool
	Verdict  RiskVerdict // risk assessment: low | high | uncertain
	Reason   string
	Mode     ReviewMode
	// Source names who produced the outcome: rule | mode | heuristic | llm |
	// human | degrade.
	Source string
	At     time.Time
}

// ReviewGate decides ask-effect permission requests.
type ReviewGate struct {
	mode     ReviewMode
	reviewer RiskReviewer // LLM fallback, nil = rules only
	approver Approver     // human push target, nil = no human reachable
	timeout  time.Duration
	checker  PermissionChecker // optional: explicit allow/deny rules (hard block)
}

// NewReviewGate wires a gate. escalationTimeout <= 0 falls back to
// DefaultEscalateTimeout. reviewer/approver may be nil depending on mode.
func NewReviewGate(mode ReviewMode, reviewer RiskReviewer, approver Approver, escalationTimeout time.Duration) *ReviewGate {
	if escalationTimeout <= 0 {
		escalationTimeout = DefaultEscalateTimeout
	}
	return &ReviewGate{mode: mode, reviewer: reviewer, approver: approver, timeout: escalationTimeout}
}

// WithChecker attaches the permission checker so the gate can hard-block
// explicit deny rules and fast-path explicit allow rules before reviewing.
func (g *ReviewGate) WithChecker(p PermissionChecker) *ReviewGate {
	g.checker = p
	return g
}

// Reviewer exposes the wired LLM fallback (tests/diagnostics; may be nil).
func (g *ReviewGate) Reviewer() RiskReviewer { return g.reviewer }

// Gate is the main entry: it resolves one request into an approve/deny
// decision plus the risk verdict for audit.
func (g *ReviewGate) Gate(ctx context.Context, action, resource, detail string) (bool, RiskVerdict, error) {
	d, err := g.Decide(ctx, action, resource, detail)
	return d.Approved, d.Verdict, err
}

// Decide resolves one request without a diff preview (kept for callers that
// only have action/resource/detail). Prefer DecideRequest so the human
// approver sees the pending change.
func (g *ReviewGate) Decide(ctx context.Context, action, resource, detail string) (GateDecision, error) {
	return g.DecideRequest(ctx, ApprovalRequest{Action: action, Resource: resource, Detail: detail})
}

// DecideRequest is Gate with the full audit record (reason/mode/source) and
// the change preview handed to the human approver.
func (g *ReviewGate) DecideRequest(ctx context.Context, req ApprovalRequest) (GateDecision, error) {
	// Explicit rules first: deny hard-blocks in every mode (architecture 4.2).
	if g.checker != nil {
		switch g.checker.Check(req.Action, req.Resource) {
		case EffectDeny:
			return g.outcome(false, RiskHigh, "explicit deny rule", "rule"), nil
		case EffectAllow:
			return g.outcome(true, RiskLow, "explicit allow rule", "rule"), nil
		}
	}
	switch g.mode {
	case ReviewFull:
		return g.outcome(true, RiskLow, "full mode auto-approved ask", "mode"), nil
	case ReviewHuman:
		return g.humanFirst(ctx, req)
	default: // ReviewAI (default)
		return g.aiFirst(ctx, req)
	}
}

// aiFirst runs the rule heuristic, falls back to the LLM reviewer and only
// escalates to a human when both stay uncertain (D23).
func (g *ReviewGate) aiFirst(ctx context.Context, req ApprovalRequest) (GateDecision, error) {
	verdict, reason, source := g.decideAI(ctx, req)
	switch verdict {
	case RiskLow:
		return g.outcome(true, RiskLow, reason, source), nil
	case RiskHigh:
		return g.outcome(false, RiskHigh, reason, source), nil
	}
	// Uncertain: escalate to a human exactly once; no response -> deny.
	approved, decided, err := g.waitHuman(ctx, req)
	if err != nil {
		return GateDecision{}, err
	}
	if decided {
		src := "human"
		if approved {
			return g.outcome(true, RiskUncertain, "human approved escalated ask", src), nil
		}
		return g.outcome(false, RiskUncertain, "human denied escalated ask", src), nil
	}
	return g.outcome(false, RiskUncertain,
		"escalated ask got no human decision; conservative deny (no re-escalation, D24)", "degrade"), nil
}

// humanFirst pushes to the human approver. A timeout denies the request and
// aborts the run; approver failures still degrade to the ai verdict (D24).
func (g *ReviewGate) humanFirst(ctx context.Context, req ApprovalRequest) (GateDecision, error) {
	approved, decided, err := g.waitHuman(ctx, req)
	if err != nil {
		return GateDecision{}, err
	}
	if decided {
		if approved {
			return g.outcome(true, RiskLow, "human approved", "human"), nil
		}
		return g.outcome(false, RiskHigh, "human denied", "human"), nil
	}
	verdict, reason, _ := g.decideAI(ctx, req)
	switch verdict {
	case RiskLow:
		return g.outcome(true, RiskLow, "human escalation timed out; degraded to ai: "+reason, "degrade"), nil
	case RiskHigh:
		return g.outcome(false, RiskHigh, "human escalation timed out; degraded to ai: "+reason, "degrade"), nil
	default:
		return g.outcome(false, RiskUncertain,
			"human escalation timed out; degraded to ai stayed uncertain; deny (no re-escalation, D24)", "degrade"), nil
	}
}

// decideAI applies the heuristic feature table first and only calls the LLM
// RiskReviewer when the table stays uncertain (D23).
func (g *ReviewGate) decideAI(ctx context.Context, req ApprovalRequest) (RiskVerdict, string, string) {
	if v := heuristicVerdict(req.Action, req.Resource, req.Detail); v != RiskUncertain {
		return v, "heuristic classified request as " + string(v), "heuristic"
	}
	if g.reviewer == nil {
		return RiskUncertain, "heuristic inconclusive and no llm reviewer configured", "heuristic"
	}
	v, err := g.reviewer.Review(ctx, req.Action, req.Resource, req.Detail)
	if err != nil {
		if ctx.Err() != nil {
			return RiskUncertain, "", ""
		}
		return RiskUncertain, "llm reviewer failed: " + err.Error(), "llm"
	}
	if v != RiskLow && v != RiskHigh && v != RiskUncertain {
		v = RiskUncertain
	}
	return v, "llm reviewer judged " + string(v), "llm"
}

// waitHuman asks the Approver and enforces the escalation timeout even if the
// Approver ignores ctx. decided=false means no human decision (timeout,
// missing approver or channel failure).
func (g *ReviewGate) waitHuman(ctx context.Context, req ApprovalRequest) (approved bool, decided bool, err error) {
	if g.approver == nil {
		return false, false, nil
	}
	cctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	type result struct {
		ok  bool
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ok, aerr := g.approver.Approve(cctx, req)
		ch <- result{ok, aerr}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			if ctx.Err() != nil {
				return false, false, ctx.Err()
			}
			// The approver may surface the escalation deadline through its own
			// return value instead of the select below; both mean "no answer".
			if errors.Is(r.err, context.DeadlineExceeded) {
				return false, false, ErrApprovalTimeout
			}
			return false, false, nil // approver failure: degrade to the ai verdict
		}
		return r.ok, true, nil
	case <-cctx.Done():
		if ctx.Err() != nil {
			return false, false, ctx.Err()
		}
		// No human answer: deny and stop the run (never auto-approve).
		return false, false, ErrApprovalTimeout
	}
}

func (g *ReviewGate) outcome(approved bool, verdict RiskVerdict, reason, source string) GateDecision {
	return GateDecision{
		Approved: approved,
		Verdict:  verdict,
		Reason:   reason,
		Mode:     g.mode,
		Source:   source,
		At:       time.Now().UTC(),
	}
}

// Heuristic feature tables (D23). Sources: the design examples (rm -rf,
// git push --force, sensitive-file writes = high; read file, ls, go test =
// low) plus classic destructive shell operations. The table is deliberately
// non-exhaustive: anything unmatched stays uncertain for the LLM/human layer.

var highCmdSubstrings = []string{
	"rm -rf", "rm -fr", "rm -r ", "rm -f ",
	"git push --force", "git push -f", "git reset --hard", "git clean -f",
	" -delete", "chmod 777", "mkfs", "dd if=", "> /dev/sd",
	"| sh", "| bash", "| zsh", ":(){",
}

var highCmdWords = map[string]bool{
	"rm": true, "sudo": true, "shutdown": true, "reboot": true, "halt": true,
	"mkfs": true, "fdisk": true, "dd": true, "shred": true, "wipefs": true,
	"userdel": true, "groupdel": true,
}

var lowCmdSubstrings = []string{
	"go test", "go build", "go vet", "gofmt", "go mod", "go fmt",
	"git status", "git log", "git diff", "git show", "git branch",
}

var lowCmdWords = map[string]bool{
	"ls": true, "cat": true, "pwd": true, "head": true, "tail": true,
	"grep": true, "rg": true, "find": true, "wc": true, "tree": true,
	"echo": true, "which": true, "whoami": true, "date": true, "stat": true,
	"file": true, "diff": true, "sort": true, "uniq": true,
}

// sensitivePathMarkers flag credential/config paths: writes/edits/deletes of
// them are high risk, reads fall through to the LLM/human layer.
var sensitivePathMarkers = []string{
	".ssh", ".gnupg", ".aws", ".env", "id_rsa", "id_ed25519",
	"credentials", ".netrc", "/etc/passwd", "/etc/shadow", "/etc/sudoers",
	".chyan/secret", ".git/hooks",
}

// heuristicVerdict classifies one request from the feature tables; it returns
// RiskUncertain when the tables have no opinion.
func heuristicVerdict(action, resource, detail string) RiskVerdict {
	action = normalizeAction(action)
	res := strings.ToLower(strings.TrimSpace(resource))
	sensitive := containsAny(res, sensitivePathMarkers)
	switch action {
	case "read":
		if sensitive {
			return RiskUncertain // secret reads deserve a second opinion
		}
		return RiskLow
	case "edit", "write", "delete":
		if sensitive {
			return RiskHigh // touching credential files
		}
		return RiskUncertain
	case "bash":
		switch {
		case containsAny(res, highCmdSubstrings) || cmdWordMatch(res, highCmdWords):
			return RiskHigh
		case sensitive:
			return RiskHigh // reaching for secrets from the shell
		case containsAny(res, lowCmdSubstrings) || cmdWordMatch(res, lowCmdWords):
			return RiskLow
		default:
			return RiskUncertain
		}
	default: // other tool actions (web_fetch/web_search, browser, MCP <server>_<tool>, …)
		return RiskUncertain
	}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// cmdWordMatch checks the first word of each command segment (split on
// shell separators) against a word table.
func cmdWordMatch(cmd string, words map[string]bool) bool {
	for _, seg := range strings.FieldsFunc(cmd, func(r rune) bool {
		return r == ';' || r == '&' || r == '|' || r == '\n'
	}) {
		fields := strings.Fields(seg)
		if len(fields) > 0 && words[fields[0]] {
			return true
		}
	}
	return false
}
