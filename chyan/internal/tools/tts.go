// tts.go: tts tool (core.Tool) — Kokoro speech synthesis landing audio files under the cache layout.
package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chyan.tech/chyan/internal/core"
)

const (
	defaultTTSBaseURL = "http://127.0.0.1:18765" // local Kokoro service (docker "kokoro")
	ttsSpeechPath     = "/v1/audio/speech"       // OpenAI-compatible: request JSON, response raw audio
	defaultTTSVoice   = "af_heart"
	ttsModel          = "tts-1"
	ttsDefaultTimeout = 300 * time.Second // long texts need a generous cap
	maxTTSErrorBody   = 512               // error snippet cap from a failing service
)

var ttsFormats = map[string]bool{"mp3": true, "wav": true}

// TTSTool implements core.Tool: synthesize speech through the Kokoro
// OpenAI-compatible endpoint. Audio lands in cache/<sha256(text)>/tts/
// <timestamp>-tts.<format> (cache layout, architecture 4.6) unless an
// explicit absolute path inside workDir is given.
type TTSTool struct {
	workDir  string        // confinement root for explicit output paths
	cacheDir string        // cache root for default output; empty = OS cache dir/chyan
	timeout  time.Duration // whole-request timeout
	maxOut   int           // tool output cap in bytes

	BaseURL string       // service base URL; empty falls back to local Kokoro
	Client  *http.Client // injectable for tests; nil = default client
}

// NewTTSTool builds the tts tool. cacheDir empty falls back to the OS cache
// dir; timeout <= 0 falls back to 300s; maxOut <= 0 falls back to 20000 bytes.
func NewTTSTool(workDir, cacheDir string, timeout time.Duration, maxOut int) *TTSTool {
	if cacheDir == "" {
		if d, err := os.UserCacheDir(); err == nil {
			cacheDir = filepath.Join(d, "chyan")
		}
	}
	if timeout <= 0 {
		timeout = ttsDefaultTimeout
	}
	if maxOut <= 0 {
		maxOut = 20000
	}
	return &TTSTool{workDir: workDir, cacheDir: cacheDir, timeout: timeout, maxOut: maxOut}
}

// Name implements core.Tool.
func (t *TTSTool) Name() string { return "tts" }

// Schema implements core.Tool.
func (t *TTSTool) Schema() map[string]any {
	return obj(map[string]any{
		"text":   map[string]any{"type": "string", "description": "text to synthesize"},
		"voice":  map[string]any{"type": "string", "description": "Kokoro voice name; default af_heart"},
		"format": map[string]any{"type": "string", "enum": []any{"mp3", "wav"}, "description": "audio format; default mp3"},
		"path":   map[string]any{"type": "string", "description": "absolute output path inside work_dir; default cache layout"},
	}, "text")
}

// Execute implements core.Tool.
func (t *TTSTool) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	return marshalResult(t.run(ctx, call.Input), t.maxOut)
}

func (t *TTSTool) run(ctx context.Context, in map[string]any) map[string]any {
	text, ok := strArg(in, "text")
	if !ok || strings.TrimSpace(text) == "" {
		return fail("text is required")
	}
	format := str(in, "format")
	if format == "" {
		format = "mp3"
	}
	if !ttsFormats[format] {
		return fail("format must be mp3 or wav")
	}
	voice := str(in, "voice")
	if voice == "" {
		voice = defaultTTSVoice
	}
	out, inCache, err := t.outPath(in, text, format)
	if err != nil {
		return fail(err.Error())
	}

	rctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	audio, err := t.synth(rctx, text, voice, format)
	if err != nil {
		if rctx.Err() == context.DeadlineExceeded {
			return map[string]any{
				"success":   false,
				"error":     fmt.Sprintf("tts timed out after %s", t.timeout),
				"timed_out": true,
			}
		}
		if ctx.Err() != nil {
			return fail("tts canceled")
		}
		return fail("tts: " + err.Error())
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fail("create output dir: " + err.Error())
	}
	if err := os.WriteFile(out, audio, 0o644); err != nil {
		return fail("write audio: " + err.Error())
	}
	res := map[string]any{
		"success": true,
		"path":    out,
		"format":  format,
		"voice":   voice,
		"bytes":   len(audio),
	}
	if inCache {
		res["note"] = "cache 产物 24h 后清理，重要产出请 move 到 work_dir"
	}
	return res
}

// outPath resolves the audio destination: an explicit absolute path inside
// workDir, or the content-addressed cache layout for the synthesized text.
func (t *TTSTool) outPath(in map[string]any, text, format string) (string, bool, error) {
	if p := str(in, "path"); p != "" {
		if !filepath.IsAbs(p) {
			return "", false, fmt.Errorf("path must be absolute: %s", p)
		}
		full, err := resolveIn(t.workDir, p)
		if err != nil {
			return "", false, err
		}
		return full, false, nil
	}
	if t.cacheDir == "" {
		return "", false, fmt.Errorf("no cache dir configured for tts output")
	}
	sum := sha256.Sum256([]byte(text))
	name := fmt.Sprintf("%d-tts.%s", time.Now().UnixMilli(), format)
	return filepath.Join(t.cacheDir, hex.EncodeToString(sum[:]), "tts", name), true, nil
}

// synth calls the OpenAI-compatible speech endpoint and returns the raw
// audio bytes.
func (t *TTSTool) synth(ctx context.Context, text, voice, format string) ([]byte, error) {
	payload, err := json.Marshal(map[string]string{
		"model":           ttsModel,
		"input":           text,
		"voice":           voice,
		"response_format": format,
	})
	if err != nil {
		return nil, err
	}
	base := t.BaseURL
	if base == "" {
		base = defaultTTSBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+ttsSpeechPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: t.timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxTTSErrorBody))
		return nil, fmt.Errorf("service returned %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}
	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read audio: %w", err)
	}
	if len(audio) == 0 {
		return nil, fmt.Errorf("service returned empty audio")
	}
	return audio, nil
}
