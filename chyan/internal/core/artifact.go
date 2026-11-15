// Tool-output artifacts: when an output exceeds the prompt-cache-safe cap,
// the full original is written to disk and the model-facing message keeps a
// pointer plus a content hash. The lesson from AutoResearch is that summaries
// must never become the only truth source: the raw metric/diff is the anchor
// that lets a later conclusion be verified instead of hallucinated.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// saveArtifact writes content under <dir>/<session>/<tool>/ and returns the
// absolute path plus the sha256 hex digest. Best-effort: a write failure is
// the caller's signal to fall back to plain truncation.
func saveArtifact(dir, session, tool, content string) (string, string, error) {
	if dir == "" {
		return "", "", fmt.Errorf("artifact dir is empty")
	}
	sessionDir := safeSegment(session, "session")
	toolDir := safeSegment(tool, "tool")
	target := filepath.Join(dir, sessionDir, toolDir)
	if err := os.MkdirAll(target, 0o700); err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(content))
	digest := hex.EncodeToString(sum[:])
	name := fmt.Sprintf("%d-%s.txt", time.Now().UnixNano(), digest[:12])
	path := filepath.Join(target, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", "", err
	}
	return path, digest, nil
}

// safeSegment maps an arbitrary id onto a portable path component.
func safeSegment(s, fallback string) string {
	if s == "" {
		return fallback
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 64 {
			break
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return fallback
	}
	return out
}

// Image input limits (FR-10): one image, and images per user turn.
const (
	MaxImageBytes    = 5 << 20
	MaxImagesPerTurn = 4
)

// DetectImageMIME maps the first bytes to a supported image MIME. Empty means
// "not an image format chyan accepts".
func DetectImageMIME(data []byte) string {
	switch http.DetectContentType(data) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return http.DetectContentType(data)
	default:
		return ""
	}
}

// SaveImage persists one image under <dir>/<session>/image/ and returns an
// image Part carrying the artifact pointer plus the raw bytes for the request.
// The bytes are validated and size-capped here so every caller (CLI, PWA, IM,
// read tool) shares the same limits.
func SaveImage(dir, session, mime string, data []byte) (Part, error) {
	if dir == "" {
		return Part{}, fmt.Errorf("artifact: image dir is empty")
	}
	if len(data) == 0 {
		return Part{}, fmt.Errorf("artifact: image is empty")
	}
	if len(data) > MaxImageBytes {
		return Part{}, fmt.Errorf("artifact: image is %d bytes, over the %d byte limit", len(data), MaxImageBytes)
	}
	if mime == "" {
		mime = DetectImageMIME(data)
	}
	if !strings.HasPrefix(mime, "image/") || DetectImageMIME(data) == "" {
		return Part{}, fmt.Errorf("artifact: unsupported image type %q", mime)
	}
	// The artifact keeps the original bytes; the model gets a downscaled copy
	// (long edge <= MaxImageEdge) so a phone screenshot cannot blow the
	// context or hit a provider dimension limit.
	send, sendMIME, err := PrepareImage(data)
	if err != nil {
		return Part{}, err
	}
	target := filepath.Join(dir, safeSegment(session, "session"), "image")
	if err := os.MkdirAll(target, 0o700); err != nil {
		return Part{}, err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	name := fmt.Sprintf("%d-%s%s", time.Now().UnixNano(), digest[:12], imageExt(mime))
	if err := os.WriteFile(filepath.Join(target, name), data, 0o600); err != nil {
		return Part{}, err
	}
	return Part{Type: "image", Image: &Image{
		MIME: sendMIME, Path: filepath.Join(target, name), SHA256: digest,
		Size: len(data), Data: send,
	}}, nil
}

// imageExt maps a supported MIME to its file extension.
func imageExt(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".bin"
	}
}
