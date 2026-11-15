// update_test.go — `chyan update` output against fake release endpoints.
package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// releaseServer serves GitHub-style release metadata plus the SHA256SUMS
// asset. urlOf rewrites asset URLs onto the test server.
func releaseServer(t *testing.T, tag string, withSums bool) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		assets := ""
		if withSums {
			assets = `,"assets":[
				{"name":"SHA256SUMS","browser_download_url":"` + srv.URL + `/download/SHA256SUMS"},
				{"name":"chyan_0.2.0_linux_amd64.tar.gz","browser_download_url":"` + srv.URL + `/download/chyan_0.2.0_linux_amd64.tar.gz"}
			]`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `","html_url":"` + srv.URL + `/releases/tag/` + tag + `"` + assets + `}`))
	})
	mux.HandleFunc("/download/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("2fd4e1c67a2d28fced849ee1bb76e7391b93eb12  chyan_0.2.0_linux_amd64.tar.gz\n"))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func runUpdateForTest(t *testing.T, base, current string) string {
	t.Helper()
	var out bytes.Buffer
	err := runUpdate(context.Background(), &out, updateOptions{
		releasesURL: base, current: current,
		goos: "linux", goarch: "amd64",
		hc: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	return out.String()
}

func TestRunUpdateReportsArtifactAndChecksum(t *testing.T) {
	srv := releaseServer(t, "v0.2.0", true)
	out := runUpdateForTest(t, srv.URL, "0.1.0")

	for _, want := range []string{
		"0.1.0 → v0.2.0",
		"chyan_0.2.0_linux_amd64.tar.gz",
		"/download/chyan_0.2.0_linux_amd64.tar.gz",
		"2fd4e1c67a2d28fced849ee1bb76e7391b93eb12",
		"sha256sum -c",
		"只提示不替换",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunUpdateUpToDate(t *testing.T) {
	srv := releaseServer(t, "v0.1.0", false)
	out := runUpdateForTest(t, srv.URL, "0.1.0")
	if !strings.Contains(out, "已是最新版本") {
		t.Fatalf("output = %s", out)
	}
}

func TestRunUpdateWithoutSumsAsset(t *testing.T) {
	srv := releaseServer(t, "v0.2.0", false)
	out := runUpdateForTest(t, srv.URL, "0.1.0")
	// Download URL is derived from the release page even without assets.
	if !strings.Contains(out, "/releases/download/v0.2.0/chyan_0.2.0_linux_amd64.tar.gz") {
		t.Errorf("derived download URL missing:\n%s", out)
	}
	if !strings.Contains(out, "未找到 SHA256SUMS") {
		t.Errorf("missing checksum warning:\n%s", out)
	}
}

func TestRunUpdateNetworkFailureIsFriendly(t *testing.T) {
	srv := releaseServer(t, "v0.2.0", true)
	base := srv.URL
	srv.Close() // unreachable endpoint, like an offline machine
	out := runUpdateForTest(t, base, "0.1.0")
	if !strings.Contains(out, "检查更新失败") || !strings.Contains(out, "离线升级") {
		t.Fatalf("output = %s", out)
	}
}
