// configreload_test.go — POST /api/v1/config/reload (U3 daemon side).
package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"chyan.tech/chyan/internal/api"
)

// writeDaemonConfig replaces the test daemon's config.toml.
func writeDaemonConfig(t *testing.T, home, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReloadConfigEndpointAppliesAndReports(t *testing.T) {
	s, a, _ := newTestServer(t)
	h := s.Handler()
	writeDaemonConfig(t, a.Home(), "[review]\nmode = \"human\"\n[llm]\nmodel = \"reload-model\"\n")

	w, req := doJSON(t, http.MethodPost, "/api/v1/config/reload", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var res api.ReloadResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if res.Model != "reload-model" || res.ReviewMode != "human" || res.ConfigPath == "" || res.Tools == 0 {
		t.Fatalf("result = %+v", res)
	}
	// The status endpoint reads the swapped wiring, so the new mode is visible.
	sw := getJSON(t, h, "/api/v1/status")
	var st api.Status
	if err := json.Unmarshal(sw.Body.Bytes(), &st); err != nil {
		t.Fatalf("status decode: %v", err)
	}
	if st.ReviewMode != "human" {
		t.Fatalf("status review mode = %q, want human", st.ReviewMode)
	}
}

func TestReloadConfigEndpointRejectsBadConfig(t *testing.T) {
	s, a, _ := newTestServer(t)
	h := s.Handler()
	writeDaemonConfig(t, a.Home(), "[review\nmode = \"human\"\n")

	w, req := doJSON(t, http.MethodPost, "/api/v1/config/reload", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	// The live config is untouched, so the status still reports the old mode.
	sw := getJSON(t, h, "/api/v1/status")
	var st api.Status
	_ = json.Unmarshal(sw.Body.Bytes(), &st)
	if st.ReviewMode != "ai" {
		t.Fatalf("review mode changed after a failed reload: %q", st.ReviewMode)
	}
}
