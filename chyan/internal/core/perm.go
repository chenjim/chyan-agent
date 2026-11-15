// Three-state permission rule engine (allow/ask/deny x action x resource
// glob) and sub-agent narrowing (Intersect never widens, architecture 4.2).
package core

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Rule is one permission entry: an effect on an action matching a resource
// pattern. Action is a tool name (read|write|edit|list|search|grep|delete|
// bash|browser|web_fetch|web_search|<server>_<tool>|...) matched exactly,
// or "*" for all actions.
type Rule struct {
	Effect   Effect
	Action   string
	Resource string // path/command glob: "**" any chars, "*" one segment (any char for commands), "?" one char
}

// PermSet is a rule-based PermissionChecker.
type PermSet struct {
	rules []Rule
}

// NewPermSet builds a checker from rules. Entries with invalid effects are
// dropped so a malformed config entry can never widen permissions.
func NewPermSet(rules ...Rule) *PermSet {
	p := &PermSet{}
	for _, r := range rules {
		if validEffect(r.Effect) {
			p.rules = append(p.rules, r)
		}
	}
	return p
}

var _ PermissionChecker = (*PermSet)(nil)

func validEffect(e Effect) bool {
	return e == EffectAllow || e == EffectAsk || e == EffectDeny
}

// DefaultEffect applies when no rule matches: read is open (read-only
// inspection), everything else — including bash — defaults to ask (appendix A:
// bash default ask; the review gate decides asks).
func DefaultEffect(action string) Effect {
	if normalizeAction(action) == "read" {
		return EffectAllow
	}
	return EffectAsk
}

// Check resolves one request: a literal pattern beats a wildcard pattern,
// then among equals deny > ask > allow; no match falls back to DefaultEffect.
func (p *PermSet) Check(action, resource string) Effect {
	if e, ok := p.RuleEffect(action, resource); ok {
		return e
	}
	return DefaultEffect(action)
}

// RuleEffect resolves only the explicit rules: ok=false means no configured
// rule matched. Callers that have their own default (e.g. tools which skip the
// gate) use this to honour deny/ask rules without inventing an ask default.
func (p *PermSet) RuleEffect(action, resource string) (Effect, bool) {
	action, resource = normalizeRequest(action, resource)
	var exact, glob Effect
	exactHit, globHit := false, false
	for _, r := range p.rules {
		if !actionMatch(r.Action, action) {
			continue
		}
		pat := normalizePattern(action, r.Resource)
		if !resourceMatch(action, pat, resource) {
			continue
		}
		if pat == resource { // literal pattern is the most specific match
			if !exactHit || effectRank(r.Effect) > effectRank(exact) {
				exact, exactHit = r.Effect, true
			}
			continue
		}
		if !globHit || effectRank(r.Effect) > effectRank(glob) {
			glob, globHit = r.Effect, true
		}
	}
	switch {
	case exactHit:
		return exact, true
	case globHit:
		return glob, true
	default:
		return "", false
	}
}

// narrowedPerms intersects a parent checker with a sub-agent declaration
// (map key = action or "*", value = the declared effect).
type narrowedPerms struct {
	parent   PermissionChecker
	declared map[string]Effect
}

var _ PermissionChecker = (*narrowedPerms)(nil)

// Intersect implements PermissionChecker: effective = parent ∩ declared, so a
// sub-agent can only narrow what the parent granted.
func (p *PermSet) Intersect(declared map[string]Effect) PermissionChecker {
	return &narrowedPerms{parent: p, declared: declared}
}

func (n *narrowedPerms) Check(action, resource string) Effect {
	e := n.parent.Check(action, resource)
	if d, ok := lookupDeclared(n.declared, action); ok {
		return tighten(e, d)
	}
	return e
}

func (n *narrowedPerms) Intersect(declared map[string]Effect) PermissionChecker {
	return &narrowedPerms{parent: n, declared: declared}
}

// lookupDeclared resolves the declared effect for action: a specific action
// key overrides the "*" catch-all. Invalid effects are ignored (never widen).
func lookupDeclared(declared map[string]Effect, action string) (Effect, bool) {
	action = normalizeAction(action)
	if d, ok := declared[action]; ok && validEffect(d) {
		return d, true
	}
	if d, ok := declared["*"]; ok && validEffect(d) {
		return d, true
	}
	return "", false
}

// tighten combines a parent effect with a declared one so the result is never
// wider than either side (deny > ask > allow): parent deny always stays deny,
// parent ask is never widened to allow.
func tighten(parent, declared Effect) Effect {
	if effectRank(declared) > effectRank(parent) {
		return declared
	}
	return parent
}

// effectRank orders effects for conflict resolution: deny > ask > allow.
func effectRank(e Effect) int {
	switch e {
	case EffectDeny:
		return 2
	case EffectAsk:
		return 1
	default:
		return 0
	}
}

// normalizeRequest maps an incoming action/resource pair to canonical form so
// rule and request spellings compare equal.
func normalizeRequest(action, resource string) (string, string) {
	return normalizeAction(action), normalizePattern(action, resource)
}

// normalizeAction lower-cases the action name (edit/write/delete stay
// distinct actions).
func normalizeAction(action string) string {
	return strings.ToLower(strings.TrimSpace(action))
}

// normalizePattern canonicalizes a resource or pattern: path-like actions get
// "~" expansion and path cleaning; other resources are only trimmed.
func normalizePattern(action, resource string) string {
	switch normalizeAction(action) {
	case "edit", "read", "write", "delete", "external_directory":
		return normalizePath(resource)
	default:
		return strings.TrimSpace(resource)
	}
}

// normalizePath expands a leading "~" and cleans the path (slash-separated)
// so "./x", "a//b" and "a/../b" all compare equal to their canonical form.
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = os.Getenv("HOME")
		}
		if home != "" {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	return path.Clean(filepath.ToSlash(p))
}

func actionMatch(pattern, action string) bool {
	return pattern == "*" || normalizeAction(pattern) == action
}

// resourceMatch matches a resource against a glob pattern. Path-like actions
// match per segment ("**" spans segments, "*" stays within one); command-like
// resources (bash/web/mcp) match as one string where "*" and "**" both span
// any characters.
func resourceMatch(action, pattern, resource string) bool {
	switch normalizeAction(action) {
	case "edit", "read", "write", "delete", "external_directory":
		if pattern == "*" || pattern == "**" {
			return true // catch-all spellings
		}
		return matchSegs(strings.Split(pattern, "/"), strings.Split(resource, "/"))
	default:
		return segMatch(strings.ReplaceAll(pattern, "**", "*"), resource)
	}
}

// matchSegs matches slash-separated segments with per-segment wildcards.
func matchSegs(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegs(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 || !segMatch(pat[0], segs[0]) {
		return false
	}
	return matchSegs(pat[1:], segs[1:])
}

// segMatch matches one string with star-backtracking wildcards
// ("*" any run of chars, "?" one char).
func segMatch(pattern, s string) bool {
	pi, si, star, mark := 0, 0, -1, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
