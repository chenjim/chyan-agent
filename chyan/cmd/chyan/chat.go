// chat.go — interactive chat REPL for bare `chyan`: every turn is a daemon
// task, replies stream over SSE, approval tickets are answered on the terminal,
// and built-in/project slash commands run client-side (config reload included).
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/tui"
)

// chatRunner drives one interactive conversation: reads lines from in, writes
// streamed replies to out, diagnostics to err. reader overrides the default
// input strategy (tests inject scripted readers).
type chatRunner struct {
	client *tui.Client
	in     io.Reader
	out    io.Writer
	err    io.Writer
	md     *mdRenderer
	banner bool
	reader lineReader
	// phase is the active run mode (planner or executor). Shift+Tab toggles it;
	// the empty value behaves as executor.
	phase core.Phase
	// workDir is sent with every task (absolute); the daemon owns the run.
	workDir string
	// auto approves tickets without asking (--auto).
	auto bool
	// approvals reads y/n for tickets; nil when no daemon ticket can appear.
	approvals *approvalPrompt
	// commands caches project slash commands for /help and Tab completion.
	commands []api.CommandInfo
	// agent/model are sent with every task (empty = daemon defaults).
	agent string
	model string
	// initialSession seeds the first turn ("continue" resumes the latest).
	initialSession string
}

// chatOptions carries the per-process settings a daemon task needs. agent and
// model follow the run flags; session seeds the first turn ("" = new,
// "continue" = latest).
type chatOptions struct {
	workDir string
	agent   string
	model   string
	session string
	auto    bool
}

// lineReader reads one input line after drawing the prompt (promptFn renders
// the prompt for the current phase). io.EOF ends the session (Ctrl-D / end of
// pipe); a cancelled draft returns ("", errCanceled).
type lineReader interface {
	ReadLine(promptFn func() string) (string, error)
	Close() error
}

// errCanceled means the user aborted the current draft (Ctrl-C): show a fresh
// prompt, keep the session.
var errCanceled = errors.New("input canceled")

// runChat starts the REPL against a daemon client. Returns when the user quits.
func runChat(ctx context.Context, c *tui.Client, opts chatOptions, in io.Reader, out, errw io.Writer) error {
	r := &chatRunner{client: c, in: in, out: out, err: errw,
		md: newMDRenderer(out, !term.IsTerminal(os.Stdout.Fd())), banner: true,
		workDir: opts.workDir, auto: opts.auto,
		agent: opts.agent, model: opts.model, initialSession: opts.session,
		approvals: newApprovalPrompt(in, errw, opts.auto)}
	// Best effort: help/completion degrade to built-ins if the daemon is slow.
	if cmds, err := c.ListCommands(ctx); err == nil {
		r.commands = cmds
	}
	return r.loop(ctx)
}

// loop drives the read-eval-print cycle until quit.
func (r *chatRunner) loop(ctx context.Context) error {
	in := r.reader
	if in == nil {
		in = newLineReader(r.in, r.md.raw, r.togglePhase, r.completions)
		defer in.Close()
	}
	if r.banner {
		fmt.Fprintf(r.out, "chyan 交互对话 — 输入内容直接对话，/ 补全命令（Tab），Shift+Tab 切换 Plan/Build，/help 看命令，/quit 退出\n")
	}
	session := r.initialSession // "" = new, "continue" = latest
	for {
		line, err := in.ReadLine(r.promptFn)
		switch {
		case errors.Is(err, errCanceled):
			fmt.Fprintln(r.out, "（已取消）")
			continue
		case errors.Is(err, io.EOF):
			fmt.Fprintln(r.out)
			return nil
		case err != nil:
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if quit, err := r.dispatch(ctx, line, &session); err != nil {
			fmt.Fprintf(r.err, "%s\n", r.paint("203", "error: "+err.Error()))
		} else if quit {
			return nil
		}
	}
}

// promptFn renders the input prompt for the active phase, opencode-style:
// a colored bar and phase name (Build blue, Plan orange) with a dim hint.
func (r *chatRunner) promptFn() string {
	if r.phase == core.PhasePlanner {
		return r.paint("215", "▌Plan") + " " + r.paint("244", "›") + " "
	}
	return r.paint("39", "▌Build") + " " + r.paint("244", "›") + " "
}

// togglePhase flips planner <-> executor (Shift+Tab in the input line).
func (r *chatRunner) togglePhase() {
	if r.phase == core.PhasePlanner {
		r.phase = core.PhaseExecutor
	} else {
		r.phase = core.PhasePlanner
	}
}

// dispatch handles one input line: built-in slash commands first, then the
// agent run (which expands project slash commands). session is updated in
// place after each run so /status works on the live session.
func (r *chatRunner) dispatch(ctx context.Context, line string, session *string) (quit bool, err error) {
	// Shell escape: "!cmd" runs locally without the LLM; the command and its
	// output are recorded into the session so later turns can see them.
	if strings.HasPrefix(line, "!") {
		return false, r.runShell(ctx, strings.TrimSpace(strings.TrimPrefix(line, "!")), session)
	}
	name := slashName(line)
	if strings.HasPrefix(line, "/") {
		switch name {
		case "quit", "exit", "q":
			fmt.Fprintln(r.out, "再见。")
			return true, nil
		case "help":
			r.printHelp()
			return false, nil
		case "new":
			*session = ""
			fmt.Fprintln(r.out, "已开启新会话。")
			return false, nil
		case "sessions":
			arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/sessions"))
			if arg == "" {
				return false, r.listSessions(ctx, *session)
			}
			sid, err := r.pickSession(ctx, arg)
			if err != nil {
				return false, err
			}
			*session = sid
			fmt.Fprintf(r.out, "已切换到会话 %s\n", sessionLabel(sid))
			r.printRecentHistory(ctx, sid)
			return false, nil
		case "status":
			sid := *session
			if sid == "" {
				// Same as the old in-process REPL: /status materializes a
				// session so the budget has something to describe.
				se, err := r.client.CreateSession(ctx, api.CreateSessionRequest{})
				if err != nil {
					return false, err
				}
				sid = se.ID
				*session = sid
			}
			usage, err := r.client.SessionContext(ctx, sid)
			if err != nil {
				return false, err
			}
			fmt.Fprintf(r.out, "会话 %s\n", sessionLabel(sid))
			fmt.Fprint(r.out, formatBudget(budgetOf(usage)))
			return false, nil
		case "compact":
			if *session == "" {
				fmt.Fprintln(r.out, "当前会话为空，无需压缩。")
				return false, nil
			}
			note, err := r.client.CompactSession(ctx, *session)
			if err != nil {
				return false, err
			}
			fmt.Fprint(r.out, "已压缩。\n"+formatHandoff(note))
			return false, nil
		case "image":
			arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/image"))
			if arg == "" {
				fmt.Fprintln(r.out, "用法：/image <图片路径> [问题]（png/jpg/webp/gif，≤5MB）")
				return false, nil
			}
			path, prompt := arg, ""
			if i := strings.IndexAny(arg, " \t"); i >= 0 {
				path, prompt = arg[:i], strings.TrimSpace(arg[i+1:])
			}
			images, err := loadImages([]string{path})
			if err != nil {
				return false, err
			}
			if prompt == "" {
				prompt = "请描述这张图片。"
			}
			return r.runAgent(ctx, prompt, session, images)
		case "theme":
			arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/theme"))
			if arg == "" {
				fmt.Fprintf(r.out, "当前主题：%s（可选：%s）\n", r.md.theme, "auto/dark/light/pink/ascii")
				return false, nil
			}
			if !r.md.setTheme(arg) {
				return false, fmt.Errorf("未知主题 %q（可选：auto/dark/light/pink/ascii）", arg)
			}
			fmt.Fprintf(r.out, "已切换主题：%s\n", arg)
			return false, nil
		case "reload":
			return false, r.reload()
		}
	}
	// Anything else (plain text or a project slash command) runs the agent.
	return r.runAgent(ctx, line, session, nil)
}

// reload re-reads config.toml on the daemon so [llm]/[review]/[permission]/
// [tools]/[mcp] and the skills/commands/instructions set take effect without
// restarting (U3). A config that cannot be applied keeps the running one.
func (r *chatRunner) reload() error {
	res, err := r.client.ReloadConfig(context.Background())
	if err != nil {
		return fmt.Errorf("配置未生效（沿用原配置）：%w", err)
	}
	model := res.Model
	if model == "" {
		model = "—"
	}
	fmt.Fprintf(r.out, "已重新加载 %s\n", res.ConfigPath)
	fmt.Fprintf(r.out, "  模型 %s｜审核 %s｜权限规则 %d｜技能 %d｜命令 %d｜MCP %d｜工具 %d\n",
		model, res.ReviewMode, res.PermRules, res.Skills, res.Commands, res.MCPServers, res.Tools)
	return nil
}

// runAgent renders one agent turn: create a daemon task, stream its reply and
// progress, answer approval tickets, and print the turn summary. images are
// data URLs (REPL /image, tests).
func (r *chatRunner) runAgent(ctx context.Context, line string, session *string, images []string) (bool, error) {
	// Echo the user turn first: the input line vanishes when the editor
	// exits, and a transcript without the question reads broken.
	fmt.Fprintf(r.out, "%s\n", r.paint("250", "你："+line))
	// Reply text streams through the markdown renderer (block by block); any
	// progress marker printed mid-block flushes the block first so ordering
	// stays sane.
	var progressOpen bool
	onText := func(text string) {
		if progressOpen {
			progressOpen = false
			_, _ = io.WriteString(r.out, "\n")
		}
		_, _ = r.md.Write([]byte(text))
	}
	// Progress makes the silent phases visible: thinking, tool start/finish.
	onProgress := func(ev tui.Event) {
		if r.md.Pending() {
			// keep output order: print the partial block before a marker
			r.md.Flush()
		}
		kind, _ := ev.Data["kind"].(string)
		switch kind {
		case "thinking":
			fmt.Fprint(r.out, r.paint("244", "… 思考中"))
			progressOpen = true
		case "tool_run":
			line := stringField(ev, "tool")
			if d := stringField(ev, "detail"); d != "" {
				line += ": " + d
			}
			fmt.Fprintf(r.out, "\n%s", r.paint("67", "→ "+line))
			progressOpen = true
		case "tool_done":
			status, code := "ok", "71" // green
			if stringField(ev, "err") != "" {
				status, code = "失败", "203" // red
			}
			elapsed := float64(numberField(ev, "elapsed_ms")) / 1000
			fmt.Fprintf(r.out, "  [%s %.1fs]", r.paint(code, status), elapsed)
			progressOpen = true
		case "reflexion":
			fmt.Fprintf(r.out, "\n%s", r.paint("140", "⟳ 迭代触顶，自我批判后接力 "+stringField(ev, "detail")))
			progressOpen = true
		}
	}
	// Ctrl+C while a turn runs interrupts the daemon task (LLM, tools,
	// sub-agents) and keeps the session: the editor's raw mode owns Ctrl+C only
	// between turns, so a signal here means "stop this turn", not "quit".
	ir := &interrupter{}
	stop := ir.watchTurn()
	defer stop()
	start := time.Now()
	req := api.CreateTaskRequest{
		Prompt: line, SessionID: *session, Phase: string(r.phase),
		Agent: r.agent, Model: r.model, WorkDir: r.workDir, Images: images,
	}
	approve := func(ctx context.Context, tk api.ApprovalTicket) bool {
		if r.approvals == nil {
			return false
		}
		return r.approvals.decide(ctx, tk)
	}
	task, err := runDaemonTask(ctx, r.client, req, taskHooks{
		onText: onText, onProgress: onProgress,
		onCreated: func(t api.Task) {
			ir.set(func() {
				_ = cancelTask(r.client, t.ID)
				// A visible approval prompt owns stdin: it must be answered
				// before the interrupt can be rendered.
				fmt.Fprintln(r.err, r.paint("244", "已请求中断（若正等待审批，请回答 y/N）"))
			})
		},
		approve: approve,
	})
	if errors.Is(err, core.ErrInterrupted) {
		r.md.Flush()
		if task.SessionID != "" {
			*session = task.SessionID // keep the conversation, even if this was turn one
		}
		fmt.Fprintln(r.out, r.paint("244", "（已中断）"))
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if task.SessionID != "" {
		*session = task.SessionID
	}
	r.md.Flush()
	fmt.Fprintf(r.out, "\n%s\n\n", r.paint("244", r.turnMeta(ctx, task, time.Since(start))))
	return false, nil
}

// turnMeta renders the post-turn line. Tokens are cumulative session usage
// (per-turn usage lives on the task, which the CLI API does not return).
func (r *chatRunner) turnMeta(ctx context.Context, task api.Task, elapsed time.Duration) string {
	meta := sessionLabel(task.SessionID)
	s, err := r.client.GetSession(ctx, task.SessionID)
	if err != nil {
		return meta + " · " + fmt.Sprintf("%.1fs", elapsed.Seconds())
	}
	model := s.Model
	if model == "" {
		model = "—"
	}
	meta += " · " + model + " · " + fmt.Sprintf("%.1fs", elapsed.Seconds())
	if s.Context != nil && s.Context.Used > 0 {
		meta += " · 上下文 " + tokensSize(s.Context.Used)
	}
	if s.TokensIn > 0 || s.TokensOut > 0 {
		meta += fmt.Sprintf(" · 累计 %s in / %s out", tokensSize(s.TokensIn), tokensSize(s.TokensOut))
	}
	if s.CostUSD > 0 {
		meta += fmt.Sprintf(" · $%.4f", s.CostUSD)
	}
	return meta
}

// stringField reads one string from an SSE payload.
func stringField(ev tui.Event, key string) string {
	s, _ := ev.Data[key].(string)
	return s
}

// numberField reads one number from an SSE payload (JSON numbers are float64).
func numberField(ev tui.Event, key string) float64 {
	switch v := ev.Data[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

// budgetOf maps the API context usage onto the CLI's budget formatter.
func budgetOf(u api.ContextUsage) core.Budget {
	return core.Budget{
		ContextLimit: u.ContextLimit, FixedCost: u.FixedCost,
		HardLimit: u.HardLimit, SoftLimit: u.SoftLimit,
		HistoryTokens: u.HistoryTokens, HistoryBytes: u.HistoryBytes,
	}
}

// paint wraps s in a 256-color code; output stays plain when it is not a
// terminal (pipes, scripted runs) so logs and scripts parse unchanged.
func (r *chatRunner) paint(code, s string) string {
	return paintAnsi(r.md.raw, code, s)
}

// runShell executes a user-typed shell command directly (bash escape mode).
// No approval gate is needed — the human typed it, and no LLM is called.
// The command is echoed ($ cmd — the editor line is gone) and the command
// plus its output are recorded in the daemon session as a user message
// (clamped to the tool-output budget) so the next turn sees them. The exit
// code shows on screen only when the output is empty (stderr already explains
// failures); the recorded note always carries it for the model. Timeout matches
// the bash tool default (30s).
func (r *chatRunner) runShell(ctx context.Context, cmd string, session *string) error {
	if cmd == "" {
		fmt.Fprintln(r.out, "用法：! <shell 命令> — 直接执行，命令与结果记入对话上下文")
		return nil
	}
	fmt.Fprintln(r.out, r.paint("244", "$ "+cmd))
	runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(runCtx, "bash", "-c", cmd)
	out, err := c.CombinedOutput()
	if len(out) > 0 {
		if !bytes.HasSuffix(out, []byte("\n")) {
			out = append(out, '\n')
		}
		_, _ = r.out.Write(out)
	}
	note := string(out)
	if err != nil {
		switch {
		case runCtx.Err() == context.DeadlineExceeded:
			fmt.Fprintln(r.out, r.paint("203", "（30s 超时中止）"))
			note += "\n(30s 超时中止)"
		default:
			code := 1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
			if len(out) == 0 {
				fmt.Fprintln(r.out, r.paint("203", fmt.Sprintf("[退出码 %d]", code)))
			}
			note += fmt.Sprintf("\n[退出码 %d]", code)
		}
	}
	sid := *session
	if sid == "" {
		se, err := r.client.CreateSession(ctx, api.CreateSessionRequest{})
		if err != nil {
			return err
		}
		sid = se.ID
	}
	sid, err = r.client.AppendNote(ctx, sid, cmd, note)
	if err != nil {
		return err
	}
	*session = sid
	return nil
}

// sessionLabel renders a session id for display ("ses1a2b3c4d5e6f7g8").
func sessionLabel(sid string) string {
	if sid == "" {
		return "（新会话）"
	}
	return sid
}

// listSessions prints the recent sessions; /sessions <n|id> switches to one.
// A star marks the live session (empty = the latest one the run resolves to).
func (r *chatRunner) listSessions(ctx context.Context, current string) error {
	sessions, err := r.client.ListSessions(ctx)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Fprintln(r.out, "暂无历史会话。")
		return nil
	}
	// The listing stays compact; /sessions <id> may still reach older rows.
	if len(sessions) > 10 {
		sessions = sessions[:10]
	}
	for i, s := range sessions {
		marker := " "
		if s.ID == current || (current == "" && i == 0) {
			marker = "*"
		}
		title := r.sessionTitle(ctx, s.ID, s.Title)
		fmt.Fprintf(r.out, "%s %2d. %s  %s  %s\n", marker, i+1, s.ID, s.UpdatedAt.Format("01-02 15:04"), title)
	}
	fmt.Fprintln(r.out, "输入 /sessions <序号|ID> 切换。")
	return nil
}

// sessionTitle labels a session for the listing: stored title first, else the
// first user message (read-side fallback — no writes, old sessions benefit
// too). Deciding a title needs the conversation, so it is never guessed at
// creation time.
func (r *chatRunner) sessionTitle(ctx context.Context, sid, stored string) string {
	if t := strings.TrimSpace(stored); t != "" {
		return truncateRunes(t, 30)
	}
	s, err := r.client.SessionTranscript(ctx, sid, 5, false)
	if err != nil {
		return "—"
	}
	for _, m := range s.Messages {
		if m.Role != "user" {
			continue
		}
		// Shell notes carry a newline and output; a title is the first line.
		if t := firstLine(m.Text); t != "" {
			return truncateRunes(t, 30)
		}
	}
	return "—"
}

// truncateRunes cuts s to n runes with an ellipsis when longer.
func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// pickSession resolves a /sessions argument: listing index (1-based), full
// id, or a unique id prefix.
func (r *chatRunner) pickSession(ctx context.Context, arg string) (string, error) {
	sessions, err := r.client.ListSessions(ctx)
	if err != nil {
		return "", err
	}
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(sessions) {
			return "", fmt.Errorf("序号 %d 超出范围（1-%d）", n, len(sessions))
		}
		return sessions[n-1].ID, nil
	}
	var prefixHits []string
	for _, s := range sessions {
		if s.ID == arg {
			return s.ID, nil
		}
		if strings.HasPrefix(s.ID, arg) {
			prefixHits = append(prefixHits, s.ID)
		}
	}
	switch len(prefixHits) {
	case 1:
		return prefixHits[0], nil
	case 0:
		return "", fmt.Errorf("没有匹配的会话：%s", arg)
	default:
		return "", fmt.Errorf("前缀 %q 匹配 %d 个会话，请用更长的 ID 或序号", arg, len(prefixHits))
	}
}

// printHelp lists built-in commands plus the loaded project commands.
func (r *chatRunner) printHelp() {
	fmt.Fprint(r.out, `内置命令：
  /help              显示本帮助
  /status            上下文窗口占用
  /compact           手动压缩（handoff 交接）
  /theme [名称]      切换 markdown 渲染主题（auto/dark/light/pink/ascii）
  /reload            重新读取 config.toml（LLM/审核/权限/工具/MCP/技能立即生效）
  /new               开启新会话
  /sessions [序号|ID] 列出最近会话 / 切换会话
  /image <路径> [问题] 附加图片提问（png/jpg/webp/gif，≤5MB）
  /quit              退出（Ctrl-D 同效）

快捷键：
  Shift+Tab          切换 Plan/Build（Plan 只读规划写入计划，Build 按计划执行）
  ! <命令>           输入 ! 进入 Shell 模式（prompt 变黄），直接执行命令；
                     退格退出回到之前阶段，命令与结果记入对话上下文
`)
	if len(r.commands) == 0 {
		fmt.Fprint(r.out, "\n（未发现 .agents/commands 自定义斜杠命令）\n")
		return
	}
	fmt.Fprint(r.out, "\n自定义命令（.agents/commands/*.md）：\n")
	for _, c := range r.commands {
		fmt.Fprintf(r.out, "  /%-16s %s\n", c.Name, firstLine(c.Description))
	}
}

// slashName returns the "/name" token without the slash and arguments.
func slashName(s string) string {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "/"))
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln
		}
	}
	return ""
}

// completions lists every supported slash command for Tab completion: the
// built-ins plus the loaded project commands.
func (r *chatRunner) completions() []string {
	out := []string{"/help", "/status", "/compact", "/theme", "/image", "/new", "/sessions", "/reload", "/quit"}
	for _, c := range r.commands {
		out = append(out, "/"+c.Name)
	}
	return out
}

// printRecentHistory replays the latest turns after a session switch so the
// terminal regains the conversation context. Tool bookkeeping and empty
// messages are skipped; long text is clamped per message.
func (r *chatRunner) printRecentHistory(ctx context.Context, sid string) {
	if sid == "" {
		fmt.Fprintln(r.out, "（该会话暂无历史内容）")
		return
	}
	s, err := r.client.SessionTranscript(ctx, sid, 40, true)
	if err != nil || len(s.Messages) == 0 {
		fmt.Fprintln(r.out, "（该会话暂无历史内容）")
		return
	}
	fmt.Fprintln(r.out, "最近历史：")
	printed := 0
	for _, m := range s.Messages {
		if printed >= 20 {
			break
		}
		text := strings.TrimSpace(m.Text)
		if text == "" {
			continue
		}
		switch m.Role {
		case "user":
			fmt.Fprintf(r.out, "  你：%s\n", truncateRunes(text, 200))
			printed++
		case "assistant":
			fmt.Fprintf(r.out, "  助手：%s\n", truncateRunes(text, 200))
			printed++
		}
	}
}
