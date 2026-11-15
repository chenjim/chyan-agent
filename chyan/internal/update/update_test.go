package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serveJSON returns a handler that always responds 200 with the given body.
func serveJSON(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

func TestLatest(t *testing.T) {
	srv := httptest.NewServer(serveJSON(`{"tag_name":"v0.2.0","html_url":"https://example.com/r","published_at":"2026-10-01T00:00:00Z"}`))
	defer srv.Close()

	c := NewChecker(srv.URL, "0.1.0", srv.Client())
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.TagName != "v0.2.0" {
		t.Errorf("TagName = %q, want v0.2.0", rel.TagName)
	}
	if rel.HTMLURL != "https://example.com/r" {
		t.Errorf("HTMLURL = %q", rel.HTMLURL)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); !rel.PublishedAt.Equal(want) {
		t.Errorf("PublishedAt = %v, want %v", rel.PublishedAt, want)
	}
}

func TestCheckUpdateAvailable(t *testing.T) {
	srv := httptest.NewServer(serveJSON(`{"tag_name":"v0.2.0","html_url":"u"}`))
	defer srv.Close()

	c := NewChecker(srv.URL, "0.1.0", srv.Client())
	info, err := c.CheckUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if info == nil {
		t.Fatal("CheckUpdate = nil, want update info")
	}
	if info.Current != "0.1.0" || info.Latest.TagName != "v0.2.0" {
		t.Errorf("info = %+v", info)
	}
}

func TestCheckUpdateUpToDate(t *testing.T) {
	srv := httptest.NewServer(serveJSON(`{"tag_name":"v0.1.0","html_url":"u"}`))
	defer srv.Close()

	c := NewChecker(srv.URL, "0.1.0", srv.Client())
	info, err := c.CheckUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if info != nil {
		t.Errorf("CheckUpdate = %+v, want nil (already latest)", info)
	}
}

func TestLatestBadJSON(t *testing.T) {
	srv := httptest.NewServer(serveJSON(`{"tag_name":`))
	defer srv.Close()

	c := NewChecker(srv.URL, "0.1.0", srv.Client())
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("Latest: want error for bad JSON, got nil")
	}
}

func TestLatestEmptyTag(t *testing.T) {
	srv := httptest.NewServer(serveJSON(`{"html_url":"u"}`))
	defer srv.Close()

	c := NewChecker(srv.URL, "0.1.0", srv.Client())
	if _, err := c.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "tag_name") {
		t.Fatalf("Latest: want empty tag_name error, got %v", err)
	}
}

func TestLatestBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewChecker(srv.URL, "0.1.0", srv.Client())
	if _, err := c.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("Latest: want status error, got %v", err)
	}
}

func TestLatestNetworkError(t *testing.T) {
	srv := httptest.NewServer(serveJSON(`{}`))
	url := srv.URL
	srv.Close() // connection refused from here on

	c := NewChecker(url, "0.1.0", &http.Client{Timeout: time.Second})
	if _, err := c.CheckUpdate(context.Background()); err == nil {
		t.Fatal("CheckUpdate: want network error, got nil")
	}
}

func TestLatestTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := NewChecker(srv.URL, "0.1.0", &http.Client{Timeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := c.Latest(context.Background())
	if err == nil {
		t.Fatal("Latest: want timeout error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout took %v, want prompt failure", elapsed)
	}
}

func TestLatestRequestError(t *testing.T) {
	// A control character in the URL makes NewRequest fail before any I/O.
	c := NewChecker("http://bad\x7furl", "0.1.0", nil)
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("Latest: want request build error, got nil")
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"v0.1.0", "0.1.0", 0}, // v prefix optional
		{"0.2.0", "0.1.0", 1},
		{"0.1.0", "0.2.0", -1},
		{"v1.0.0", "v1", 0}, // missing fields count as zero
		{"1.0.1", "1", 1},
		{"0.10.0", "0.9.0", 1}, // numeric, not lexicographic
		{"1.2.3.4", "1.2.3", 1},
		// Non-semver tags fall back to string comparison.
		{"nightly", "0.1.0", 1},
		{"0.1.0", "nightly", -1},
		// Pre-releases rank below their release (SemVer precedence).
		{"0.1.0-rc1", "0.1.0", -1},
		{"0.1.0", "0.1.0-rc1", 1},
		{"0.1.0-rc.2", "0.1.0-rc.10", -1},
		{"0.1.0-alpha", "0.1.0-beta", -1},
		{"0.1.0-2", "0.1.0-alpha", -1}, // numeric < alphanumeric
		{"v0.1.0+build7", "0.1.0", 0},  // build metadata ignored
		{"abc", "abc", 0},
		{"", "0.1.0", -1},
		{"0.1.", "0.1.0", -1}, // trailing dot is non-numeric -> string compare (prefix is less)
	}
	for _, tt := range tests {
		if got := CompareVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestNewCheckerTrimsSlash(t *testing.T) {
	srv := httptest.NewServer(serveJSON(`{"tag_name":"v0.1.0"}`))
	defer srv.Close()

	c := NewChecker(srv.URL+"/", "0.1.0", srv.Client())
	if _, err := c.Latest(context.Background()); err != nil {
		t.Fatalf("Latest with trailing slash baseURL: %v", err)
	}
}

func TestArtifactNames(t *testing.T) {
	if got := ArtifactName("0.2.0", "linux", "amd64"); got != "chyan_0.2.0_linux_amd64" {
		t.Errorf("ArtifactName linux = %q", got)
	}
	if got := ArtifactName("0.2.0", "windows", "arm64"); got != "chyan_0.2.0_windows_arm64.exe" {
		t.Errorf("ArtifactName windows = %q", got)
	}
	if got := ArchiveName("0.2.0", "windows", "amd64"); got != "chyan_0.2.0_windows_amd64.zip" {
		t.Errorf("ArchiveName windows = %q", got)
	}
	if got := ArchiveName("0.2.0", "darwin", "arm64"); got != "chyan_0.2.0_darwin_arm64.tar.gz" {
		t.Errorf("ArchiveName darwin = %q", got)
	}
}

func TestSumFor(t *testing.T) {
	sums := "aaa  chyan_0.2.0_linux_amd64.tar.gz\nbbb *chyan_0.2.0_darwin_arm64.tar.gz\n\n"
	if got, ok := SumFor(sums, "chyan_0.2.0_linux_amd64.tar.gz"); !ok || got != "aaa" {
		t.Fatalf("SumFor linux = %q %v", got, ok)
	}
	if got, ok := SumFor(sums, "chyan_0.2.0_darwin_arm64.tar.gz"); !ok || got != "bbb" {
		t.Fatalf("SumFor star line = %q %v", got, ok)
	}
	if _, ok := SumFor(sums, "missing.tgz"); ok {
		t.Fatal("SumFor should miss unknown files")
	}
}

func TestArchiveURL(t *testing.T) {
	rel := Release{
		TagName: "v0.2.0",
		HTMLURL: "https://github.com/o/r/releases/tag/v0.2.0",
		Assets:  []Asset{{Name: "a.tar.gz", BrowserDownloadURL: "https://cdn/a.tar.gz"}},
	}
	if got := rel.ArchiveURL("a.tar.gz"); got != "https://cdn/a.tar.gz" {
		t.Errorf("asset URL = %q", got)
	}
	if got := rel.ArchiveURL("b.tar.gz"); got != "https://github.com/o/r/releases/download/v0.2.0/b.tar.gz" {
		t.Errorf("derived URL = %q", got)
	}
	rel.HTMLURL = "not a release page"
	if got := rel.ArchiveURL("b"); got != "" {
		t.Errorf("unexpected derived URL = %q", got)
	}
}

func TestFetchText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sums":
			_, _ = w.Write([]byte("deadbeef  chyan_0.2.0_linux_amd64.tar.gz\n"))
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewChecker(srv.URL, "0.1.0", srv.Client())
	body, err := c.FetchText(context.Background(), srv.URL+"/sums")
	if err != nil || !strings.Contains(body, "deadbeef") {
		t.Fatalf("FetchText = %q, %v", body, err)
	}
	if _, err := c.FetchText(context.Background(), srv.URL+"/big"); err == nil {
		t.Fatal("oversized manifest should be rejected")
	}
	if _, err := c.FetchText(context.Background(), srv.URL+"/missing"); err == nil {
		t.Fatal("404 should be an error")
	}
}
