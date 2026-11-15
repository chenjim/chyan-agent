package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"chyan.tech/chyan/internal/core"
)

// dangerRule is one dangerous-command blacklist entry (adapted from the
// legacy pattern table from the earlier Python implementation). Word rules are
// anchored at command position (start or after ; | &) to avoid flagging
// arguments like "echo sudo"; the destructive patterns stay broad.
type dangerRule struct {
	name    string
	pattern string
	re      *regexp.Regexp
}

// commandAnchor anchors a word rule at a command position: start of input or
// after a shell separator. Newline counts as a separator, otherwise a second
// line ("echo ok\nsudo rm -rf /") would slip past the blacklist.
const commandAnchor = `(?:^|[;&|\n]\s*)`

var dangerRules = []dangerRule{
	{name: "rm-root", pattern: `\brm\s+(-{1,2}[a-zA-Z-]+\s+)+/\S*`},
	{name: "rm-home", pattern: `\brm\s+(-{1,2}[a-zA-Z-]+\s+)+~\S*`},
	{name: "mkfs", pattern: commandAnchor + `mkfs(\.[a-zA-Z0-9]+)?\b`},
	{name: "dd-to-device", pattern: `\bdd\s+[^;&\n]*\bof=/dev/`},
	{name: "fork-bomb", pattern: `:\(\)\s*\{[^}]*\}\s*;?\s*:`},
	{name: "system-power", pattern: commandAnchor + `(shutdown|reboot|halt|poweroff)\b`},
	{name: "sudo", pattern: commandAnchor + `sudo\b`},
	{name: "redirect-blockdev", pattern: `>\s*/dev/(sd|nvme|hd|xvd|vd)`},
	{name: "chmod-root", pattern: `\bchmod\s+-R\s+777\s+/(\s|$)`},
	{name: "pipe-to-shell", pattern: `\b(curl|wget)\b[^|;&\n]*\|\s*(ba|z|d|k)?sh\b`},
	{name: "git-force-push", pattern: `\bgit\s+push\b[^;&\n]*\s(-f|--force)(\s|$)`},
}

func init() {
	for i := range dangerRules {
		dangerRules[i].re = regexp.MustCompile(dangerRules[i].pattern)
	}
}

// matchDangerous returns the name of the first matching danger rule.
func matchDangerous(command string) (string, bool) {
	for _, r := range dangerRules {
		if r.re.MatchString(command) {
			return r.name, true
		}
	}
	return "", false
}

// BashTool implements core.Tool: run a command under bash inside work_dir
// with a dangerous-command blacklist, timeout and output cap.
type BashTool struct {
	workDir string
	timeout time.Duration // default execution timeout
	maxOut  int           // tool output cap in bytes
}

// NewBashTool builds the bash tool. timeout <= 0 falls back to 30s,
// maxOut <= 0 falls back to 20000 bytes.
func NewBashTool(workDir string, timeout time.Duration, maxOut int) *BashTool {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxOut <= 0 {
		maxOut = 20000
	}
	return &BashTool{workDir: workDir, timeout: timeout, maxOut: maxOut}
}

// Name implements core.Tool.
func (t *BashTool) Name() string { return "bash" }

// Schema implements core.Tool.
func (t *BashTool) Schema() map[string]any {
	return obj(map[string]any{
		"command": map[string]any{"type": "string", "description": "shell command to run"},
		"cwd":     map[string]any{"type": "string", "description": "working dir relative to work_dir; default work_dir"},
		"timeout": map[string]any{"type": "integer", "description": "timeout seconds; default from config"},
	}, "command")
}

// Execute implements core.Tool. stdout and stderr are merged into
// "output"; the exit code is always reported.
func (t *BashTool) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	res := t.run(ctx, call.Input)
	return marshalResult(res, t.maxOut)
}

func (t *BashTool) run(ctx context.Context, in map[string]any) map[string]any {
	command, ok := strArg(in, "command")
	if !ok || command == "" {
		return fail("command is required")
	}
	if name, hit := matchDangerous(command); hit {
		return map[string]any{
			"success": false,
			"error":   fmt.Sprintf("command rejected: matches dangerous pattern %q", name),
			"rule":    name,
		}
	}
	cwd := t.workDir
	if p := str(in, "cwd"); p != "" {
		resolved, err := resolveIn(t.workDir, p)
		if err != nil {
			return fail(err.Error())
		}
		cwd = resolved
	}
	timeout := t.timeout
	if secs := intArg(in, "timeout", 0); secs > 0 {
		timeout = time.Duration(secs) * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var buf cappedBuffer
	buf.max = t.maxOut * 32 // bound worst-case collection; marshalResult caps later
	cmd := exec.CommandContext(rctx, "bash", "-c", command)
	cmd.Dir = cwd
	// A timeout must kill the whole pipeline, not just bash: `cmd | tail`
	// leaves the pipe open and Cmd.Wait blocks on the copier goroutine
	// forever. Own process group + WaitDelay bounds that to a few seconds.
	setupProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second
	// exec copies stdout and stderr from two goroutines concurrently: they
	// must not share one bytes.Buffer (data race corrupts it and crashes the
	// process on large output). cappedBuffer is mutex-guarded and bounded.
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	output := buf.String()

	if rctx.Err() == context.DeadlineExceeded {
		res := map[string]any{
			"success":   false,
			"error":     fmt.Sprintf("command timed out after %s", timeout),
			"timed_out": true,
			"output":    output,
		}
		capField(res, "output", t.maxOut)
		return res
	}
	if ctx.Err() != nil {
		return map[string]any{"success": false, "error": "command canceled", "output": output}
	}
	code := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			code = ee.ExitCode()
		} else {
			return map[string]any{"success": false, "error": "command failed: " + runErr.Error(), "output": output}
		}
	}
	res := map[string]any{
		"success":   code == 0,
		"exit_code": code,
		"output":    output,
	}
	capField(res, "output", t.maxOut)
	return res
}

// cappedBuffer is a concurrency-safe, size-bounded output collector for
// exec.Cmd (its stdout/stderr copier goroutines write concurrently).
type cappedBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	max  int
	full bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.max > 0 && b.buf.Len() >= b.max {
		b.full = true
		return len(p), nil // swallow: never block the child process
	}
	if b.max > 0 && b.buf.Len()+len(p) > b.max {
		_, _ = b.buf.Write(p[:b.max-b.buf.Len()])
		b.full = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	if b.full {
		s += "\n…[output capped]"
	}
	return s
}
