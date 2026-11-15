// Package update: checks a GitHub-releases-style endpoint for newer chyan
// versions and reports the matching artifact + SHA256. It never downloads or
// installs anything (D33): the user validates and upgrades by hand.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Asset is one downloadable release artifact.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Release is a single release entry, shaped after the GitHub releases API
// (only the fields we need).
type Release struct {
	TagName     string    `json:"tag_name"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets,omitempty"`
}

// AssetURL returns the download URL of the named asset, or "".
func (r Release) AssetURL(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

// Version strips the leading "v" from the release tag (file names carry the
// plain version, matching scripts/release.sh).
func (r Release) Version() string { return strings.TrimPrefix(r.TagName, "v") }

// ArchiveURL returns the download URL for file: the matching asset when the
// API lists one, otherwise derived from a GitHub-style html_url
// (.../releases/tag/<tag> → .../releases/download/<tag>/<file>). Empty when
// neither is available.
func (r Release) ArchiveURL(file string) string {
	if u := r.AssetURL(file); u != "" {
		return u
	}
	const tagSep = "/releases/tag/"
	if i := strings.Index(r.HTMLURL, tagSep); i >= 0 {
		return r.HTMLURL[:i] + "/releases/download/" + r.HTMLURL[i+len(tagSep):] + "/" + file
	}
	return ""
}

// ArtifactName is the binary name release.sh produces for a platform:
// chyan_<version>_<os>_<arch> (plus .exe on Windows).
func ArtifactName(version, goos, goarch string) string {
	name := fmt.Sprintf("chyan_%s_%s_%s", version, goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// ArchiveName is the published archive for a platform: zip on Windows,
// tar.gz elsewhere. It is also the file name SHA256SUMS lists.
func ArchiveName(version, goos, goarch string) string {
	if goos == "windows" {
		return fmt.Sprintf("chyan_%s_%s_%s.zip", version, goos, goarch)
	}
	return fmt.Sprintf("chyan_%s_%s_%s.tar.gz", version, goos, goarch)
}

// SumsAsset is the checksum manifest name published by release.sh.
const SumsAsset = "SHA256SUMS"

// SumFor extracts the hex digest for file from SHA256SUMS content
// ("<sha256>  <file>" lines; a leading "*" is tolerated).
func SumFor(contents, file string) (string, bool) {
	for _, line := range strings.Split(contents, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == file {
			return fields[0], true
		}
	}
	return "", false
}

// UpdateInfo describes an available update: the current and latest versions.
type UpdateInfo struct {
	Current string
	Latest  Release
}

// Checker queries <baseURL>/releases/latest and compares against the running
// version. Safe for concurrent use if the underlying client is.
type Checker struct {
	baseURL        string
	currentVersion string
	hc             *http.Client
}

// NewChecker builds a Checker. hc may be nil, defaulting to
// http.DefaultClient; the caller owns the client's timeout settings.
func NewChecker(baseURL, currentVersion string, hc *http.Client) *Checker {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Checker{
		baseURL:        strings.TrimRight(baseURL, "/"),
		currentVersion: currentVersion,
		hc:             hc,
	}
}

// Latest fetches the most recent release. ctx bounds the request.
func (c *Checker) Latest(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/releases/latest", nil)
	if err != nil {
		return Release{}, fmt.Errorf("update: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("update: fetch latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("update: unexpected status %d from %s", resp.StatusCode, req.URL)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("update: decode release: %w", err)
	}
	if rel.TagName == "" {
		return Release{}, errors.New("update: release has empty tag_name")
	}
	return rel, nil
}

// CheckUpdate returns info about a strictly newer release, or nil when already
// up to date. Network/parse errors are returned as errors.
func (c *Checker) CheckUpdate(ctx context.Context) (*UpdateInfo, error) {
	rel, err := c.Latest(ctx)
	if err != nil {
		return nil, err
	}
	if CompareVersions(rel.TagName, c.currentVersion) <= 0 {
		return nil, nil
	}
	return &UpdateInfo{Current: c.currentVersion, Latest: rel}, nil
}

// FetchText downloads a small text asset (the SHA256SUMS manifest) and
// returns its contents. The response is capped so a hostile or misconfigured
// endpoint cannot exhaust memory.
func (c *Checker) FetchText(ctx context.Context, url string) (string, error) {
	const maxBytes = 1 << 20
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("update: build request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("update: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("update: unexpected status %d from %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("update: read %s: %w", url, err)
	}
	if len(body) > maxBytes {
		return "", fmt.Errorf("update: %s exceeds %d bytes", url, maxBytes)
	}
	return string(body), nil
}

// CompareVersions compares two version strings (tags) with SemVer precedence.
// An optional leading "v" is ignored, build metadata ("+sha") is ignored, and
// a pre-release suffix ranks BELOW its release (1.2.3-rc1 < 1.2.3). Numeric
// cores compare field by field (missing fields count as zero); anything
// non-numeric falls back to plain string comparison. Returns -1, 0, or +1.
func CompareVersions(a, b string) int {
	ac, apre, aok := parseSemver(a)
	bc, bpre, bok := parseSemver(b)
	if !aok || !bok {
		return strings.Compare(strings.TrimPrefix(a, "v"), strings.TrimPrefix(b, "v"))
	}
	for i := 0; i < len(ac) || i < len(bc); i++ {
		x, y := 0, 0
		if i < len(ac) {
			x = ac[i]
		}
		if i < len(bc) {
			y = bc[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case apre == "" && bpre == "":
		return 0
	case apre == "": // release outranks its pre-releases
		return 1
	case bpre == "":
		return -1
	}
	return comparePrerelease(apre, bpre)
}

// parseSemver splits "v<core>[-<pre>][+<build>]" into numeric fields and the
// pre-release suffix. ok is false when the core is not all-numeric.
func parseSemver(s string) (core []int, pre string, ok bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	core, ok = parseNumeric(s)
	return core, pre, ok
}

// comparePrerelease compares dot-separated identifiers per SemVer: numeric
// identifiers compare numerically and rank below alphanumeric ones; a shorter
// identifier list ranks lower when all preceding identifiers are equal.
func comparePrerelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		if i >= len(as) {
			return -1
		}
		if i >= len(bs) {
			return 1
		}
		x, xn := parseNumeric(as[i])
		y, yn := parseNumeric(bs[i])
		switch {
		case xn && yn:
			if x[0] != y[0] {
				if x[0] < y[0] {
					return -1
				}
				return 1
			}
		case xn:
			return -1 // numeric identifiers rank below alphanumeric
		case yn:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	return 0
}

// parseNumeric splits a dot-separated all-numeric version into fields. ok is
// false for empty or non-numeric segments.
func parseNumeric(s string) (fields []int, ok bool) {
	if s == "" {
		return nil, false
	}
	for _, part := range strings.Split(s, ".") {
		n := 0
		if part == "" {
			return nil, false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return nil, false
			}
			n = n*10 + int(ch-'0')
		}
		fields = append(fields, n)
	}
	return fields, true
}
