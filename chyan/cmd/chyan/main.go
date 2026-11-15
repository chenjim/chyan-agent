// chyan CLI entrypoint: non-interactive runs plus status/compact commands.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/config"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/daemon"
	"chyan.tech/chyan/internal/logging"
	"chyan.tech/chyan/internal/tui"
	"chyan.tech/chyan/internal/update"
)

const version = "0.1.0-dev"

func main() {
	// Daily log files under ~/.chyan/logs (best effort; CHYAN_LOG_LEVEL=off
	// disables the file mirror). stderr keeps the raw output.
	if home, err := config.Home(); err == nil {
		defer logging.Setup(home, logging.DefaultKeepDays)()
	}
	if err := rootCmd().Execute(); err != nil {
		// A user interrupt is not a failure: exit 130 (128+SIGINT) lets
		// scripts and cron tell "canceled" apart from "broke".
		if errors.Is(err, core.ErrInterrupted) {
			fmt.Fprintln(os.Stderr, "interrupted")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	f := &runFlags{}
	root := &cobra.Command{
		Use:   "chyan",
		Short: "chyan — a personal agent",
		Long:  "chyan — a personal agent. Run with no arguments for the interactive chat.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				// `chyan "task"` = one-shot run, mirroring `chyan run`.
				return runOnce(cmd, f, args[0])
			}
			return chat(cmd, f)
		},
	}
	bindRunFlags(root, f)
	root.AddCommand(runCmd(), serveCmd(), tuiCmd(), statusCmd(), compactCmd(),
		backupCmd(), restoreCmd(), diagnoseCmd(), versionCmd(), updateCmd(), pruneCmd(), vacuumCmd(),
		exportCmd(), importCmd(), cronCmd(), memoryCmd(), traceCmd())
	return root
}

// chat launches the interactive conversation REPL as a daemon client.
func chat(cmd *cobra.Command, f *runFlags) error {
	c, err := connectDaemon(cmd.Context(), f.home, !f.noAutostart)
	if err != nil {
		return err
	}
	workDir, err := absWorkDir(f.workDir)
	if err != nil {
		return err
	}
	return runChat(cmd.Context(), c, chatOptions{
		workDir: workDir, agent: f.agent, model: f.model, session: f.session, auto: f.auto,
	}, os.Stdin, os.Stdout, os.Stderr)
}

// runOnce executes one non-interactive task (shared by `chyan run` and
// `chyan "task"`): create a daemon task, stream its output and honour
// --format json. Approvals are answered on this terminal unless --auto.
func runOnce(cmd *cobra.Command, f *runFlags, prompt string) error {
	c, err := connectDaemon(cmd.Context(), f.home, !f.noAutostart)
	if err != nil {
		return err
	}
	workDir, err := absWorkDir(f.workDir)
	if err != nil {
		return err
	}
	images, err := loadImages(f.images)
	if err != nil {
		return err
	}
	req := api.CreateTaskRequest{
		Prompt: prompt, SessionID: f.session, Agent: f.agent,
		Model: f.model, WorkDir: workDir, Images: images,
	}
	switch {
	case f.fork != "":
		req.ForkFrom = f.fork
		req.SessionID = ""
	case f.continuE:
		req.SessionID = "continue"
	}
	sink := runOutput(os.Stdout, f.format)
	streamed := false
	onText := func(text string) {
		if text != "" {
			streamed = true
		}
		if sink != nil {
			sink(text)
		}
	}
	// Ctrl+C interrupts the task (tools and sub-agents included) instead of
	// killing the process; main maps ErrInterrupted to exit code 130.
	ir := &interrupter{}
	stop := ir.watchTurn()
	defer stop()
	prompter := newApprovalPrompt(os.Stdin, os.Stderr, f.auto)
	task, err := runDaemonTask(cmd.Context(), c, req, taskHooks{
		onText: onText,
		onCreated: func(t api.Task) {
			ir.set(func() {
				_ = cancelTask(c, t.ID)
				fmt.Fprintln(os.Stderr, "已请求中断（若正等待审批，请回答 y/N）")
			})
		},
		approve: prompter.decide,
	})
	if err != nil {
		return err
	}
	if f.format == "json" {
		return printJSON(task.SessionID, task.Reply)
	}
	// A broken SSE stream may have missed the live deltas; the task row still
	// carries the complete reply.
	if !streamed && task.Reply != "" {
		fmt.Print(task.Reply)
	}
	fmt.Println()
	return nil
}

// absWorkDir resolves the tool workspace for a task: --work-dir when set, else
// this process's cwd (the daemon's own cwd must not leak into CLI runs).
func absWorkDir(flag string) (string, error) {
	if flag == "" {
		return os.Getwd()
	}
	return filepath.Abs(flag)
}

// tuiCmd launches the interactive terminal UI (P3-4) against a running daemon.
func tuiCmd() *cobra.Command {
	var (
		baseURL string
		token   string
	)
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Interactive terminal UI (talks to a running chyan serve)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if token == "" {
				token = os.Getenv("CHYAN_TOKEN")
			}
			if baseURL == "" {
				// Platform-aware default: unix socket on Linux/macOS (the OS
				// user is the trust boundary), TCP + token on Windows (no
				// local socket face, D30).
				if runtime.GOOS == "windows" {
					baseURL = os.Getenv("CHYAN_URL")
					if baseURL == "" {
						baseURL = "http://127.0.0.1:31415"
					}
					home, err := config.Home()
					if err != nil {
						return err
					}
					// Tokens stay off disk (FR-11), so the TCP face still needs
					// an explicit token before it can be auto-started.
					if token == "" {
						return errors.New("tui on Windows needs --token or CHYAN_TOKEN (the daemon has no local socket face)")
					}
					if _, err := connectDaemonToken(cmd.Context(), home, true, token); err != nil {
						return err
					}
				} else {
					home, err := config.Home()
					if err != nil {
						return err
					}
					// Start the daemon when it is down, same as the other CLI
					// entry points.
					if _, err := connectDaemon(cmd.Context(), home, true); err != nil {
						return err
					}
					baseURL = "http://unix" + filepath.Join(home, "chyan.sock")
				}
			}
			return tui.Run(cmd.Context(), baseURL, token)
		},
	}
	cmd.Flags().StringVar(&baseURL, "url", "", "daemon URL (default: local unix socket, or http://127.0.0.1:31415 on Windows)")
	cmd.Flags().StringVar(&token, "token", "", "bearer token (or CHYAN_TOKEN)")
	return cmd
}

// runFlags holds the shared session/tool flags for run-like commands.
type runFlags struct {
	home     string
	agent    string
	session  string
	workDir  string
	format   string
	auto     bool
	model    string
	fork     string
	continuE bool     // --continue (resume latest session)
	images   []string // --image (repeatable)
	// noAutostart fails instead of spawning `chyan serve` (tests/debugging).
	noAutostart bool
}

func runCmd() *cobra.Command {
	f := &runFlags{}
	cmd := &cobra.Command{
		Use:   `run "task"`,
		Short: "Run one task non-interactively",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOnce(cmd, f, args[0])
		},
	}
	bindRunFlags(cmd, f)
	return cmd
}

// serveCmd runs the daemon (HTTP REST + SSE) until interrupted.
func serveCmd() *cobra.Command {
	var (
		tcpAddr     string
		token       string
		home        string
		auto        bool
		maxTasks    int
		taskTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the chyan daemon (HTTP API + SSE)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The approval hub is the human approval channel for the app
			// gate and the ticket source for the HTTP approval endpoints.
			hub := daemon.NewApprovalHub(nil)
			a, err := app.New(app.Options{
				Home: home, Approver: hub, Auto: auto,
			})
			if err != nil {
				return err
			}
			defer a.Close()

			st := app.StoreOf(a)
			// [daemon] config supplies the defaults; flags win when set.
			limit, timeout := a.DaemonLimits()
			if maxTasks > 0 {
				limit = maxTasks
			}
			if taskTimeout > 0 {
				timeout = taskTimeout
			}
			srv, err := daemon.New(daemon.Options{
				Home: a.Home(), TCPAddr: tcpAddr, Token: token, App: a, Store: st, Gate: hub,
				MaxConcurrentTasks: limit, TaskTimeout: timeout,
			})
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// IM channels relay inbound messages into the daemon SSE hub
			// (webhook feeds the merged stream only after Start).
			if reg := a.Channels(); reg != nil {
				if err := reg.Start(ctx); err != nil {
					log.Printf("channels start degraded: %v", err)
				}
			}
			// Dream pass: nightly memory maintenance at daemon start
			// (architecture 4.3; best-effort, never blocks serving).
			a.DreamOnce(ctx)
			// Scheduled jobs run as agent turns and deliver to their channel
			// (P4-1/P4-2 glue); first run migrates the hermes jobs.json.
			sched := startScheduler(ctx, a, a.Home(), srv.CronRun)
			srv.SetScheduler(sched)
			defer sched.Stop()
			// TCP only binds when a token is set (daemon 4.5); say so plainly
			// or the user assumes 31415 is up when it is not.
			switch {
			case runtime.GOOS == "windows":
				log.Printf("chyan daemon starting (tcp=%s, token auth; no local socket on Windows)", tcpAddr)
			case token == "":
				log.Printf("chyan daemon starting (unix socket only; set --token to open TCP %s)", tcpAddr)
			default:
				log.Printf("chyan daemon starting (unix socket + tcp=%s, token auth)", tcpAddr)
			}
			return srv.Start(ctx)
		},
	}
	cmd.Flags().StringVar(&tcpAddr, "tcp-addr", "127.0.0.1:31415", "remote TCP listener (disabled when --token is empty)")
	cmd.Flags().StringVar(&token, "token", os.Getenv("CHYAN_TOKEN"), "bearer token for the remote listener")
	cmd.Flags().StringVar(&home, "home", "", "chyan data root (default ~/.chyan)")
	cmd.Flags().BoolVar(&auto, "auto", false, "auto-approve asks (review mode full; explicit deny still blocks)")
	cmd.Flags().IntVar(&maxTasks, "max-tasks", 0, "concurrent background tasks (default: [daemon] max_concurrent_tasks, else 4)")
	cmd.Flags().DurationVar(&taskTimeout, "task-timeout", 0, "per-task run timeout (default: [daemon] task_timeout, else 30m)")
	return cmd
}

func statusCmd() *cobra.Command {
	f := &runFlags{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show context window usage and data footprint",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := connectDaemon(cmd.Context(), f.home, !f.noAutostart)
			if err != nil {
				return err
			}
			sid := f.session
			if sid == "" || sid == "continue" {
				// "continue" has no path form: resolve it from the listing.
				sessions, err := c.ListSessions(cmd.Context())
				if err != nil {
					return err
				}
				if len(sessions) > 0 {
					sid = sessions[0].ID
				} else {
					sid = ""
				}
			}
			if sid == "" {
				fmt.Println("（暂无会话）")
			} else {
				usage, err := c.SessionContext(cmd.Context(), sid)
				if err != nil {
					return err
				}
				fmt.Print(formatBudget(budgetOf(usage)))
			}
			st, _ := c.GetStatus(cmd.Context())
			fmt.Printf("运行中任务：%d\n", st.RunningTasks)
			home, err := resolveHome(f.home)
			if err != nil {
				return err
			}
			fmt.Printf("数据目录 %s\n", home)
			fmt.Printf("  chyan.db  %s\n", humanBytes(fileSize(filepath.Join(home, "chyan.db"))))
			fmt.Printf("  artifacts %s\n", humanBytes(dirSize(filepath.Join(home, "artifacts"))))
			fmt.Printf("  cache     %s\n", humanBytes(dirSize(filepath.Join(home, "cache"))))
			return nil
		},
	}
	cmd.Flags().StringVar(&f.session, "session", "continue", "session id (or \"continue\")")
	cmd.Flags().StringVar(&f.home, "home", "", "chyan data root (default ~/.chyan)")
	cmd.Flags().BoolVar(&f.noAutostart, "no-autostart", false, "fail instead of starting chyan serve when the daemon is down")
	return cmd
}

func compactCmd() *cobra.Command {
	f := &runFlags{}
	cmd := &cobra.Command{
		Use:   "compact",
		Short: "Fold session history into a handoff note",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := connectDaemon(cmd.Context(), f.home, !f.noAutostart)
			if err != nil {
				return err
			}
			note, err := c.CompactSession(cmd.Context(), f.session)
			if err != nil {
				return err
			}
			fmt.Print(formatHandoff(note))
			return nil
		},
	}
	cmd.Flags().StringVar(&f.session, "session", "continue", "session id (or \"continue\")")
	cmd.Flags().StringVar(&f.home, "home", "", "chyan data root (default ~/.chyan)")
	cmd.Flags().BoolVar(&f.noAutostart, "no-autostart", false, "fail instead of starting chyan serve when the daemon is down")
	return cmd
}

func versionCmd() *cobra.Command {
	var (
		check    bool
		releases string
	)
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version (or check for updates)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Println("chyan", version)
			if !check {
				return nil
			}
			base := releases
			if base == "" {
				base = os.Getenv("CHYAN_RELEASES_URL")
			}
			if base == "" {
				return fmt.Errorf("update check needs --releases-url or CHYAN_RELEASES_URL")
			}
			info, err := update.NewChecker(base, version, nil).CheckUpdate(cmd.Context())
			if err != nil {
				return err
			}
			if info == nil {
				fmt.Println("already up to date")
				return nil
			}
			fmt.Printf("update available: %s -> %s\n  %s\n", info.Current, info.Latest.TagName, info.Latest.HTMLURL)
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check-update", false, "check the releases feed for a newer version")
	cmd.Flags().StringVar(&releases, "releases-url", "", "releases API base (or CHYAN_RELEASES_URL)")
	return cmd
}

func bindRunFlags(cmd *cobra.Command, f *runFlags) {
	cmd.Flags().StringVar(&f.home, "home", "", "chyan data root (default ~/.chyan, or CHYAN_HOME)")
	cmd.Flags().StringVar(&f.agent, "agent", "", "agent definition name (.agents/agents/<name>.md)")
	cmd.Flags().StringVar(&f.session, "session", "", "session id (empty = new, \"continue\" = latest)")
	cmd.Flags().StringVar(&f.workDir, "work-dir", "", "tool work directory (default: cwd)")
	cmd.Flags().StringVar(&f.format, "format", "text", "output format: text|json")
	cmd.Flags().BoolVar(&f.auto, "auto", false, "auto-approve daemon approval tickets (explicit deny rules still block)")
	cmd.Flags().StringVar(&f.model, "model", "", "model override (empty = config default)")
	cmd.Flags().StringVar(&f.fork, "fork", "", "fork this session into a fresh one and run there")
	cmd.Flags().BoolVar(&f.continuE, "continue", false, "continue the latest session (alias of --session continue)")
	cmd.Flags().StringArrayVar(&f.images, "image", nil, "attach an image (png/jpg/webp/gif, repeatable, max 4)")
	cmd.Flags().BoolVar(&f.noAutostart, "no-autostart", false, "fail instead of starting chyan serve when the daemon is down")
}

// runResult is the JSON shape for `run --format json`.
type runResult struct {
	Session string `json:"session_id"`
	Reply   string `json:"reply"`
}

func printJSON(sessionID, reply string) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(runResult{Session: sessionID, Reply: reply})
}

// formatHandoff renders every handoff section (core.HandoffNote has nine;
// showing only Intent/Summary hid the task list in Active/Next). Empty
// sections are skipped.
func formatHandoff(n core.HandoffNote) string {
	var sb strings.Builder
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&sb, "%s:\n", title)
		for _, it := range items {
			fmt.Fprintf(&sb, "  - %s\n", it)
		}
	}
	if n.Intent != "" {
		fmt.Fprintf(&sb, "intent: %s\n", n.Intent)
	}
	section("corrections", n.Corrections)
	section("decisions", n.Decisions)
	section("done", n.Done)
	section("active", n.Active)
	section("next", n.Next)
	section("references", n.References)
	section("lessons", n.Lessons)
	if n.Summary != "" {
		fmt.Fprintf(&sb, "summary:\n%s\n", indentBlock(n.Summary, "  "))
	}
	return sb.String()
}

// indentBlock prefixes every line of s with pad.
func indentBlock(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, ln := range lines {
		lines[i] = pad + ln
	}
	return strings.Join(lines, "\n")
}

// formatBudget renders window usage for humans: compact units, a usage bar
// and the two thresholds that matter (compression trigger, hard cap). "已用"
// estimates the next call's prompt (fixed cost + history), matching the
// per-turn "上下文" line; the history-only size stays visible for detail.
func formatBudget(b core.Budget) string {
	pct := func(n int) float64 {
		if b.ContextLimit <= 0 {
			return 0
		}
		return float64(n) / float64(b.ContextLimit) * 100
	}
	total := b.FixedCost + b.HistoryTokens
	var sb strings.Builder
	fmt.Fprintf(&sb, "上下文窗口  %s\n", tokensSize(b.ContextLimit))
	fmt.Fprintf(&sb, "  已用      %8s  %5.1f%%  %s（历史 %s tok + 固定 %s）\n",
		tokensSize(total), pct(total), usageBar(pct(total)), tokensSize(b.HistoryTokens), tokensSize(b.FixedCost))
	fmt.Fprintf(&sb, "  压缩线    %8s  %5.1f%%  超过触发 handoff 压缩\n", tokensSize(b.SoftLimit), pct(b.SoftLimit))
	fmt.Fprintf(&sb, "  硬上限    %8s  %5.1f%%\n", tokensSize(b.HardLimit), pct(b.HardLimit))
	fmt.Fprintf(&sb, "  历史体积  %8s\n", byteSize(b.HistoryBytes))
	return sb.String()
}

// usageBar draws a 24-cell progress bar for a percentage.
func usageBar(pct float64) string {
	const cells = 24
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(pct/100*cells + 0.5)
	return strings.Repeat("█", filled) + strings.Repeat("░", cells-filled)
}

// tokensSize formats a token count as 999 / 4.7k / 1.00M.
func tokensSize(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.Itoa(n)
	}
}

// byteSize formats a byte count as 171 B / 4.7 KB / 2.1 MB.
func byteSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// runOutput returns the live token sink for one run. Text format streams
// deltas as they arrive; JSON keeps stdout machine-clean (no sink, encode
// only). The caller prints the final newline.
func runOutput(w io.Writer, format string) func(string) {
	if format == "json" {
		return nil
	}
	return func(text string) { _, _ = io.WriteString(w, text) }
}
