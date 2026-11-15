package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "secret"))
}

func TestPutGetList(t *testing.T) {
	s := newStore(t)
	if err := s.Put("llm_key", "sk-abc\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("mcp.token", "tok"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("llm_key")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-abc" { // trailing whitespace trimmed
		t.Errorf("Get = %q", got)
	}
	names, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Errorf("List = %v", names)
	}
	if s.Dir() == "" {
		t.Error("Dir empty")
	}
}

func TestPutPermissions(t *testing.T) {
	s := newStore(t)
	if err := s.Put("tok", "v"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(s.dir, "tok"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
	// overwrite keeps 0600 even if the file was loosened in between
	if err := os.Chmod(filepath.Join(s.dir, "tok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("tok", "v2"); err != nil {
		t.Fatal(err)
	}
	st, _ = os.Stat(filepath.Join(s.dir, "tok"))
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm after overwrite = %o, want 600", perm)
	}
}

func TestValidateNames(t *testing.T) {
	s := newStore(t)
	bad := []string{"", "..", "../evil", "a/b", `a\b`, "/abs", ".hidden", "-lead"}
	for _, name := range bad {
		if err := s.Put(name, "v"); err == nil {
			t.Errorf("Put(%q) must fail", name)
		}
		if _, err := s.Get(name); err == nil {
			t.Errorf("Get(%q) must fail", name)
		}
	}
	good := []string{"a", "tok-1", "x_y.z", "A9"}
	for _, name := range good {
		if err := s.Put(name, "v"); err != nil {
			t.Errorf("Put(%q) = %v", name, err)
		}
	}
}

func TestGetMissing(t *testing.T) {
	s := newStore(t)
	_, err := s.Get("nope")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("want not-found error, got %v", err)
	}
}

func TestListEmptyDir(t *testing.T) {
	s := newStore(t)
	names, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("List = %v", names)
	}
}

func TestResolveSecrets(t *testing.T) {
	s := newStore(t)
	if err := s.Put("llm_key", "sk-123"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, raw, want string
		wantErr         bool
	}{
		{"plain text unchanged", "Bearer abc", "Bearer abc", false},
		{"single ref", "secret:llm_key", "sk-123", false},
		{"embedded ref", "Authorization: Bearer secret:llm_key!", "Authorization: Bearer sk-123!", false},
		{"two refs", "secret:llm_key/secret:llm_key", "sk-123/sk-123", false},
		{"missing ref", "secret:ghost", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ResolveSecrets(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveSecretsUnknownSuffix(t *testing.T) {
	s := newStore(t)
	if err := s.Put("a", "A"); err != nil {
		t.Fatal(err)
	}
	// "secret:a-b" is a valid name that does not exist -> error, not a pass-through
	if _, err := s.ResolveSecrets("secret:a-b"); err == nil {
		t.Error("want error for missing referenced secret")
	}
	// bare "secret:" without a name is left alone
	got, err := s.ResolveSecrets("just secret: text")
	if err != nil {
		t.Fatal(err)
	}
	if got != "just secret: text" {
		t.Errorf("got %q", got)
	}
}
