// Always-on router (P1-5 spike): one tool-less session per channel keeps
// cross-thread continuity and rewrites a thread's first IM request into a task
// instruction for the thread worker. It never executes work itself, so a
// router mistake cannot touch files or run shell commands. Disabled by
// default; set CHYAN_ROUTER=1 to enable.
package daemon

import (
	"context"
	"log"
	"os"
	"strings"
	"unicode/utf8"

	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/core"
)

// RouterEnv enables the router layer ("1", "true", "on", "yes").
const RouterEnv = "CHYAN_ROUTER"

// maxRouterInstruction bounds the rewritten task text so a runaway model
// cannot inflate every worker turn.
const maxRouterInstruction = 4000

// routerSystemPrompt makes the router a dispatcher, not an agent: it has no
// tools and only outputs an instruction.
const routerSystemPrompt = `你是一个常驻的调度 Router，不直接执行任务，也没有任何工具。
你的唯一职责：把 IM 里收到的用户请求改写为一条清晰、可执行的任务指令，交给后续的执行 Agent。
规则：
1. 只输出任务指令正文，不要解释、不要问候、不要提问、不要编造执行结果。
2. 保留用户给出的全部约束（路径、时间、格式、数量、语言）。
3. 信息不足时按最合理的默认假设补充，并在指令末尾用“假设：…”一行注明。
4. 若请求包含多个独立目标，按优先级拆成编号步骤。`

// router is the daemon-side dispatcher state; nil means disabled.
type router struct {
	server *Server
}

// routerEnabled reports whether the router layer is switched on.
func routerEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RouterEnv))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// imRouterKey maps one channel to its continuous router session.
func imRouterKey(channel string) string { return "im:router:" + channel }

// route rewrites a cold-start request into a worker instruction. Errors and
// empty output return "" so the caller keeps the original prompt.
func (r *router) route(ctx context.Context, channel, prompt string) string {
	var sid string
	if raw, err := r.server.store.GetKV(ctx, imRouterKey(channel)); err == nil {
		sid = strings.TrimSpace(string(raw))
	}
	msg, newSID, err := r.server.app.RunOpts(ctx, sid, "IM 请求：\n"+prompt, app.RunOptions{
		NoTools: true, SystemOverride: routerSystemPrompt,
	})
	if err != nil {
		log.Printf("router: channel %s rewrite failed: %v", channel, err)
		return ""
	}
	if newSID != "" && newSID != sid {
		if err := r.server.store.SetKV(ctx, imRouterKey(channel), []byte(newSID)); err != nil {
			log.Printf("router: persist session %s: %v", channel, err)
		}
	}
	out := strings.TrimSpace(core.FlattenText(msg))
	if out == "" {
		return ""
	}
	if utf8.RuneCountInString(out) > maxRouterInstruction {
		runes := []rune(out)
		out = string(runes[:maxRouterInstruction]) + "\n…(指令已截断)"
	}
	return out
}
