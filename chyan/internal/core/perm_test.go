package core

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPermWildcardMatrix covers path/command glob matching across actions.
func TestPermWildcardMatrix(t *testing.T) {
	p := NewPermSet(
		Rule{Effect: EffectDeny, Action: "read", Resource: "/etc/passwd"},
		Rule{Effect: EffectAllow, Action: "read", Resource: "**/*.go"},
		Rule{Effect: EffectAsk, Action: "read", Resource: "docs/*"},
		Rule{Effect: EffectDeny, Action: "write", Resource: "**/.ssh/**"},
		Rule{Effect: EffectAllow, Action: "bash", Resource: "go *"},
		Rule{Effect: EffectDeny, Action: "bash", Resource: "*rm -rf*"},
		Rule{Effect: EffectAllow, Action: "web", Resource: "https://example.com/*"},
		Rule{Effect: EffectDeny, Action: "external_directory", Resource: "/home/**"},
		Rule{Effect: EffectAllow, Action: "*", Resource: "playground/**"},
	)
	tests := []struct {
		name, action, resource string
		want                   Effect
	}{
		{"exact path", "read", "/etc/passwd", EffectDeny},
		{"glob go file", "read", "internal/core/perm.go", EffectAllow},
		{"glob go nested", "read", "a/b/c/d_test.go", EffectAllow},
		{"glob single segment", "read", "docs/arch.md", EffectAsk},
		{"glob does not cross segment", "read", "docs/sub/arch.md", EffectAllow},
		{"denied secret dir", "write", "/home/u/.ssh/id_rsa", EffectDeny},
		{"write elsewhere default", "write", "main.go", EffectAsk},
		{"bash go", "bash", "go test ./...", EffectAllow},
		{"bash rm -rf", "bash", "rm -rf /", EffectDeny},
		{"bash rm -rf buried", "bash", "cd /tmp && rm -rf x", EffectDeny},
		{"bash default ask", "bash", "curl http://x", EffectAsk},
		{"web prefix", "web", "https://example.com/a/b", EffectAllow},
		{"web default ask", "web", "https://other.com/", EffectAsk},
		{"extdir denied", "external_directory", "/home/u/x.txt", EffectDeny},
		{"read default allow", "read", "README.md", EffectAllow},
		{"wildcard action", "mcp", "playground/tool", EffectAllow},
		{"action alias case", "READ", "/etc/passwd", EffectDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Check(tt.action, tt.resource); got != tt.want {
				t.Errorf("Check(%q, %q) = %v, want %v", tt.action, tt.resource, got, tt.want)
			}
		})
	}
}

// TestPermNormalization covers ~ expansion and path cleaning on both sides.
func TestPermNormalization(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	p := NewPermSet(
		Rule{Effect: EffectDeny, Action: "edit", Resource: "~/secret/**"},
		Rule{Effect: EffectAllow, Action: "read", Resource: "/var/log/../log/*.log"},
		Rule{Effect: EffectAllow, Action: "read", Resource: "./rel/*"},
	)
	if got := p.Check("edit", filepath.Join(home, "secret", "a.txt")); got != EffectDeny {
		t.Errorf("expanded home edit = %v, want deny", got)
	}
	if got := p.Check("edit", "~/secret/a.txt"); got != EffectDeny {
		t.Errorf("tilde edit = %v, want deny", got)
	}
	if got := p.Check("read", "rel/x.log"); got != EffectAllow {
		t.Errorf("cleaned relative = %v, want allow", got)
	}
	if got := p.Check("read", "/var/log/sys.log"); got != EffectAllow {
		t.Errorf("cleaned absolute = %v, want allow", got)
	}
}

// TestPermPrecedence covers exact-over-glob and deny>ask>allow tie-breaks.
func TestPermPrecedence(t *testing.T) {
	p := NewPermSet(
		Rule{Effect: EffectAllow, Action: "read", Resource: "src/*"},
		Rule{Effect: EffectDeny, Action: "read", Resource: "src/secret.txt"}, // exact beats glob
		Rule{Effect: EffectAsk, Action: "bash", Resource: "git *"},
		Rule{Effect: EffectDeny, Action: "bash", Resource: "git push*"}, // deny wins over ask
		Rule{Effect: EffectDeny, Action: "bash", Resource: "*"},
		Rule{Effect: EffectAllow, Action: "bash", Resource: "ls"}, // exact allow beats wildcard deny
	)
	p2 := NewPermSet(
		Rule{Effect: EffectAsk, Action: "bash", Resource: "git *"},
	)
	if got := p.Check("read", "src/secret.txt"); got != EffectDeny {
		t.Errorf("exact beats glob = %v, want deny", got)
	}
	if got := p.Check("read", "src/other.txt"); got != EffectAllow {
		t.Errorf("glob still applies = %v, want allow", got)
	}
	if got := p.Check("bash", "git push origin main"); got != EffectDeny {
		t.Errorf("deny>ask = %v, want deny", got)
	}
	if got := p.Check("bash", "git status"); got != EffectDeny {
		t.Errorf("deny wins among wildcard hits = %v, want deny", got)
	}
	if got := p2.Check("bash", "git status"); got != EffectAsk {
		t.Errorf("ask glob alone = %v, want ask", got)
	}
	if got := p.Check("bash", "ls"); got != EffectAllow {
		t.Errorf("exact allow beats wildcard deny = %v, want allow", got)
	}
}

// TestPermDefaults pins the default effect table.
func TestPermDefaults(t *testing.T) {
	p := NewPermSet()
	tests := []struct {
		action string
		want   Effect
	}{
		{"read", EffectAllow},
		{"bash", EffectAsk},
		{"edit", EffectAsk},
		{"write", EffectAsk},
		{"delete", EffectAsk},
		{"web", EffectAsk},
		{"mcp", EffectAsk},
		{"external_directory", EffectAsk},
	}
	for _, tt := range tests {
		if got := p.Check(tt.action, "anything"); got != tt.want {
			t.Errorf("default Check(%q) = %v, want %v", tt.action, got, tt.want)
		}
	}
}

// TestPermInvalidEffects: malformed rules are dropped, never widen.
func TestPermInvalidEffects(t *testing.T) {
	p := NewPermSet(Rule{Effect: "maybe", Action: "bash", Resource: "*"})
	if got := p.Check("bash", "rm -rf /"); got != EffectAsk {
		t.Errorf("invalid effect rule = %v, want ask", got)
	}
}

// TestIntersectMatrix pins the tightening matrix: effective = parent ∩ child,
// deny always deny, allow×ask=ask, ask×allow=ask (never widened).
func TestIntersectMatrix(t *testing.T) {
	parent := NewPermSet(
		Rule{Effect: EffectDeny, Action: "delete", Resource: "**"},
		Rule{Effect: EffectAllow, Action: "bash", Resource: "go *"},
		Rule{Effect: EffectAsk, Action: "bash", Resource: "git *"},
		Rule{Effect: EffectAllow, Action: "read", Resource: "**"},
	)
	tests := []struct {
		name     string
		declared map[string]Effect
		action   string
		resource string
		want     Effect
	}{
		{"parent deny x child allow stays deny", map[string]Effect{"delete": EffectAllow}, "delete", "a.txt", EffectDeny},
		{"parent deny x child deny stays deny", map[string]Effect{"*": EffectDeny}, "delete", "a.txt", EffectDeny},
		{"parent allow x child ask = ask", map[string]Effect{"bash": EffectAsk}, "bash", "go test", EffectAsk},
		{"parent allow x child deny = deny", map[string]Effect{"bash": EffectDeny}, "bash", "go test", EffectDeny},
		{"parent allow x child allow = allow", map[string]Effect{"bash": EffectAllow}, "bash", "go test", EffectAllow},
		{"parent ask x child allow = ask (not widened)", map[string]Effect{"bash": EffectAllow}, "bash", "git status", EffectAsk},
		{"parent ask x child ask = ask", map[string]Effect{"bash": EffectAsk}, "bash", "git status", EffectAsk},
		{"parent ask x child deny = deny", map[string]Effect{"bash": EffectDeny}, "bash", "git status", EffectDeny},
		{"parent default ask x child deny = deny", map[string]Effect{"web": EffectDeny}, "web", "https://x", EffectDeny},
		{"parent default allow x child deny = deny", map[string]Effect{"read": EffectDeny}, "read", "a.txt", EffectDeny},
		{"star declaration narrows all", map[string]Effect{"*": EffectAsk}, "read", "a.txt", EffectAsk},
		{"action key beats star", map[string]Effect{"*": EffectDeny, "read": EffectAllow}, "read", "a.txt", EffectAllow},
		{"no declaration passthrough", nil, "bash", "go test", EffectAllow},
		{"undeclared action passthrough", map[string]Effect{"web": EffectDeny}, "bash", "go test", EffectAllow},
		{"invalid declared ignored", map[string]Effect{"bash": "maybe"}, "bash", "go test", EffectAllow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parent.Intersect(tt.declared).Check(tt.action, tt.resource)
			if got != tt.want {
				t.Errorf("Intersect(%v).Check(%q, %q) = %v, want %v", tt.declared, tt.action, tt.resource, got, tt.want)
			}
		})
	}
}

// TestIntersectChains: nested narrowing keeps tightening monotonically.
func TestIntersectChains(t *testing.T) {
	parent := NewPermSet(Rule{Effect: EffectAllow, Action: "bash", Resource: "**"})
	child := parent.Intersect(map[string]Effect{"bash": EffectAsk})
	grand := child.Intersect(map[string]Effect{"bash": EffectAllow})
	if got := grand.Check("bash", "go test"); got != EffectAsk {
		t.Errorf("chained narrow = %v, want ask (never widened)", got)
	}
	grandDeny := child.Intersect(map[string]Effect{"bash": EffectDeny})
	if got := grandDeny.Check("bash", "go test"); got != EffectDeny {
		t.Errorf("chained deny = %v, want deny", got)
	}
}
