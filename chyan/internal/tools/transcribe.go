// transcribe.go: transcribe tool (core.Tool) — submit audio to the local transcriber service and poll for the transcript.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"chyan.tech/chyan/internal/core"
)

const (
	defaultTranscribeBaseURL = "http://localhost:8080" // local transcriber-service (SenseVoice)
	transcribeSubmitPath     = "/api/v1/transcribe"    // POST multipart: file, optional title/refine
	transcribeTaskPath       = "/api/v1/task/"         // GET {task_id}: pending|processing|done|error
	transcribeDefaultTimeout = 15 * time.Minute
	transcribePollInterval   = 5 * time.Second // service-documented cadence: poll every 3-5s
	maxAudioBytes            = 100 << 20       // service upload cap for media files
)

// TranscribeTool implements core.Tool: submit a local audio/video file to
// the transcriber service, then poll the task endpoint until the transcript
// is ready (or the ctx deadline hits).
type TranscribeTool struct {
	workDir  string        // confinement root for input files
	timeout  time.Duration // whole-operation deadline (submit + polling)
	maxOut   int           // tool output cap in bytes
	interval time.Duration // poll cadence; <= 0 falls back to transcribePollInterval

	BaseURL string       // service base URL; empty falls back to local default
	Client  *http.Client // injectable for tests; nil = default client
}

// NewTranscribeTool builds the transcribe tool. timeout <= 0 falls back to
// 15min, maxOut <= 0 falls back to 20000 bytes.
func NewTranscribeTool(workDir string, timeout time.Duration, maxOut int) *TranscribeTool {
	if timeout <= 0 {
		timeout = transcribeDefaultTimeout
	}
	if maxOut <= 0 {
		maxOut = 20000
	}
	return &TranscribeTool{workDir: workDir, timeout: timeout, maxOut: maxOut}
}

// Name implements core.Tool.
func (t *TranscribeTool) Name() string { return "transcribe" }

// Schema implements core.Tool. language is forwarded to the service when
// set; the current engine (SenseVoice) auto-detects and ignores it.
func (t *TranscribeTool) Schema() map[string]any {
	return obj(map[string]any{
		"path":     map[string]any{"type": "string", "description": "audio/video file inside work_dir"},
		"language": map[string]any{"type": "string", "description": "hint language code; default auto-detect"},
	}, "path")
}

// Execute implements core.Tool.
func (t *TranscribeTool) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	return marshalResult(t.run(ctx, call.Input), t.maxOut)
}

func (t *TranscribeTool) run(ctx context.Context, in map[string]any) map[string]any {
	p, ok := strArg(in, "path")
	if !ok || strings.TrimSpace(p) == "" {
		return fail("path is required")
	}
	full, err := resolveIn(t.workDir, p)
	if err != nil {
		return fail(err.Error())
	}
	st, err := os.Stat(full)
	if err != nil {
		return fail("open audio: " + err.Error())
	}
	if !st.Mode().IsRegular() {
		return fail("not a regular file: " + p)
	}
	if st.Size() == 0 {
		return fail("audio file is empty: " + p)
	}
	if st.Size() > maxAudioBytes {
		return fail("audio file exceeds " + strconv.Itoa(maxAudioBytes>>20) + "MB upload cap: " + p)
	}

	rctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	taskID, err := t.submit(rctx, full, str(in, "language"))
	if err != nil {
		return t.httpFail(rctx, ctx, "submit", err)
	}
	text, lastStatus, err := t.wait(rctx, taskID)
	if err != nil {
		if rctx.Err() == context.DeadlineExceeded {
			return map[string]any{
				"success":   false,
				"error":     fmt.Sprintf("transcribe timed out after %s, task %s still %s", t.timeout, taskID, lastStatus),
				"timed_out": true,
				"task_id":   taskID,
			}
		}
		if ctx.Err() != nil {
			return fail("transcribe canceled")
		}
		return fail("transcribe: " + err.Error())
	}
	res := map[string]any{"success": true, "task_id": taskID, "text": text}
	capField(res, "text", t.maxOut)
	return res
}

// httpFail maps a transport/service error onto the result envelope,
// distinguishing timeout and cancellation from plain failures.
func (t *TranscribeTool) httpFail(rctx, ctx context.Context, stage string, err error) map[string]any {
	if rctx.Err() == context.DeadlineExceeded {
		return map[string]any{
			"success":   false,
			"error":     fmt.Sprintf("transcribe timed out after %s during %s", t.timeout, stage),
			"timed_out": true,
		}
	}
	if ctx.Err() != nil {
		return fail("transcribe canceled")
	}
	return fail(stage + ": " + err.Error())
}

// submit uploads the file as multipart form "file" and returns the task id
// (POST /api/v1/transcribe -> 202 {task_id}).
func (t *TranscribeTool) submit(ctx context.Context, file, language string) (string, error) {
	fh, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if language != "" {
		_ = w.WriteField("language", language) // hint only; SenseVoice auto-detects
	}
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.Base(file)))
	hdr.Set("Content-Type", "application/octet-stream")
	part, err := w.CreatePart(hdr)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, fh); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	resp, err := t.do(ctx, http.MethodPost, transcribeSubmitPath, body.Bytes(), w.FormDataContentType())
	if err != nil {
		return "", fmt.Errorf("service unreachable: %w", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxTTSErrorBody))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("service returned %d: %s", resp.StatusCode, bytes.TrimSpace(payload))
	}
	var sub struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(payload, &sub); err != nil {
		return "", fmt.Errorf("bad JSON from submit: %s", bytes.TrimSpace(payload))
	}
	if sub.TaskID == "" {
		return "", fmt.Errorf("no task_id in submit response: %s", bytes.TrimSpace(payload))
	}
	return sub.TaskID, nil
}

// wait polls GET /api/v1/task/{id} until a terminal state and returns the
// transcript text plus the last observed status.
func (t *TranscribeTool) wait(ctx context.Context, taskID string) (text, lastStatus string, err error) {
	interval := t.interval
	if interval <= 0 {
		interval = transcribePollInterval
	}
	for {
		status, md, err := t.poll(ctx, taskID)
		if err != nil {
			return "", lastStatus, err
		}
		lastStatus = status
		switch status {
		case "done":
			return md, status, nil
		case "error":
			return "", status, fmt.Errorf("task %s failed: %s", taskID, md)
		case "pending", "processing":
			// keep waiting
		default:
			return "", status, fmt.Errorf("unexpected task status %q", status)
		}
		select {
		case <-ctx.Done():
			return "", lastStatus, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// poll fetches the task once and returns its status; for terminal states
// the second value is the transcript ("done") or the error text ("error").
func (t *TranscribeTool) poll(ctx context.Context, taskID string) (status, payload string, err error) {
	resp, err := t.do(ctx, http.MethodGet, transcribeTaskPath+taskID, nil, "")
	if err != nil {
		return "", "", fmt.Errorf("service unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return "", "", fmt.Errorf("task %s not found or expired", taskID)
	}
	if resp.StatusCode/100 != 2 {
		return "", "", fmt.Errorf("service returned %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	var task struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Result struct {
			Markdown string `json:"markdown"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &task); err != nil {
		return "", "", fmt.Errorf("bad JSON from task endpoint: %s", bytes.TrimSpace(raw))
	}
	switch task.Status {
	case "done":
		return task.Status, task.Result.Markdown, nil
	case "error":
		return task.Status, task.Error, nil
	default:
		return task.Status, "", nil
	}
}

// do issues one request against the configured base URL.
func (t *TranscribeTool) do(ctx context.Context, method, path string, body []byte, contentType string) (*http.Response, error) {
	base := t.BaseURL
	if base == "" {
		base = defaultTranscribeBaseURL
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, rd)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: t.timeout}
	}
	return client.Do(req)
}
