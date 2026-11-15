package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesIndex(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "html") {
		t.Fatalf("GET / body = %q, want html", rec.Body.String())
	}
}

func TestHandlerFallsBackToIndex(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/abc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sessions/abc = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "html") {
		t.Fatalf("fallback body = %q, want html", rec.Body.String())
	}
}
