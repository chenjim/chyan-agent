// daemon.go — CLI ⇄ daemon connection. The CLI is a client: run/REPL/status/
// compact all go over the REST+SSE API instead of assembling an app in-process.
// When nothing answers on the local socket, `chyan serve` is started detached
// and the command waits for /healthz (CHYAN_NO_AUTOSTART=1 or --no-autostart
// turns the spawn off for debugging).
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"chyan.tech/chyan/internal/config"
	"chyan.tech/chyan/internal/tui"
)

const (
	// daemonStartTimeout covers extension/MCP loading on the first start.
	daemonStartTimeout = 15 * time.Second
	// daemonProbeTimeout keeps a dead socket from stalling every command.
	daemonProbeTimeout = 400 * time.Millisecond
	// daemonLogName holds the spawned daemon's stdout/stderr.
	daemonLogName = "daemon.log"
	// daemonLogMaxBytes rotates the spawn log once (daemon.log.1) before the
	// next start so a long-lived install does not grow it forever.
	daemonLogMaxBytes = 2 << 20
)

// connectDaemon returns a client for the given data root. autostart starts the
// daemon when the socket is not answering; false reports the missing daemon.
func connectDaemon(ctx context.Context, homeFlag string, autostart bool) (*tui.Client, error) {
	return connectDaemonToken(ctx, homeFlag, autostart, "")
}

// connectDaemonToken is connectDaemon with an explicit token override for the
// Windows TCP face (TUI --token). ignored on unix.
func connectDaemonToken(ctx context.Context, homeFlag string, autostart bool, tokenOverride string) (*tui.Client, error) {
	home, err := resolveHome(homeFlag)
	if err != nil {
		return nil, err
	}
	if os.Getenv("CHYAN_NO_AUTOSTART") == "1" {
		autostart = false
	}

	if runtime.GOOS == "windows" {
		// No local unix socket on Windows: TCP + token is the only face (D30).
		// Tokens are never persisted (FR-11), so auto-start needs CHYAN_TOKEN
		// (or the TUI --token flag).
		token := tokenOverride
		if token == "" {
			token = os.Getenv("CHYAN_TOKEN")
		}
		if token == "" {
			return nil, errors.New("Windows 上 CLI 需要 CHYAN_TOKEN（token 不落盘，见 FR-11）")
		}
		base := os.Getenv("CHYAN_URL")
		if base == "" {
			base = "http://127.0.0.1:31415"
		}
		c := tui.NewClient(base, token)
		if probeDaemon(ctx, c) == nil {
			return c, nil
		}
		if !autostart {
			return nil, fmt.Errorf("daemon 未启动（%s）", base)
		}
		exited, err := spawnDaemon(home, token, tcpAddrOf(base))
		if err != nil {
			return nil, err
		}
		if err := waitDaemon(ctx, c, home, exited); err != nil {
			return nil, err
		}
		return c, nil
	}

	sock := filepath.Join(home, "chyan.sock")
	c := tui.NewClient("http://unix"+sock, "")
	if probeDaemon(ctx, c) == nil {
		return c, nil
	}
	if !autostart {
		return nil, fmt.Errorf("daemon 未启动（%s）：先运行 chyan serve，或去掉 --no-autostart", sock)
	}
	exited, err := spawnDaemon(home, "", "")
	if err != nil {
		return nil, err
	}
	if err := waitDaemon(ctx, c, home, exited); err != nil {
		return nil, err
	}
	return c, nil
}

// tcpAddrOf extracts host:port from CHYAN_URL so the spawned serve listens on
// the endpoint the client will probe. Empty keeps serve's default.
func tcpAddrOf(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || !strings.Contains(u.Host, ":") {
		return ""
	}
	return u.Host
}

// resolveHome mirrors app.New: empty means $CHYAN_HOME or ~/.chyan.
func resolveHome(flagHome string) (string, error) {
	if flagHome == "" {
		return config.Home()
	}
	return filepath.Abs(flagHome)
}

// probeDaemon reports whether the daemon answers /healthz.
func probeDaemon(parent context.Context, c *tui.Client) error {
	ctx, cancel := context.WithTimeout(parent, daemonProbeTimeout)
	defer cancel()
	return c.Ping(ctx)
}

// spawnDaemon starts `chyan serve` detached from this terminal. stdout/stderr
// go to <home>/daemon.log so a failed start can explain itself. The returned
// channel reports the child's exit (nil means it is still running).
func spawnDaemon(home, token, tcpAddr string) (<-chan error, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("定位 chyan 可执行文件: %w", err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s: %w", home, err)
	}
	logPath := filepath.Join(home, daemonLogName)
	if info, err := os.Stat(logPath); err == nil && info.Size() > daemonLogMaxBytes {
		_ = os.Remove(logPath + ".1")
		_ = os.Rename(logPath, logPath+".1")
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开 %s: %w", logPath, err)
	}
	defer logf.Close()

	args := []string{"serve", "--home", home}
	if token != "" {
		args = append(args, "--token", token)
	}
	if tcpAddr != "" {
		args = append(args, "--tcp-addr", tcpAddr)
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = home
	cmd.Stdout = logf
	cmd.Stderr = logf
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 chyan serve: %w", err)
	}
	// Reap the child while this command lives; Setsid/DETACHED_PROCESS keeps it
	// alive after the CLI exits. The exit is surfaced so a broken config fails
	// in milliseconds instead of after the full startup timeout.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return exited, nil
}

// waitDaemon polls /healthz until the daemon answers, the child exits, or the
// timeout expires.
func waitDaemon(ctx context.Context, c *tui.Client, home string, exited <-chan error) error {
	return waitDaemonUntil(ctx, c, home, daemonStartTimeout, exited)
}

// waitDaemonUntil is waitDaemon with an injectable deadline (tests).
func waitDaemonUntil(ctx context.Context, c *tui.Client, home string, timeout time.Duration, exited <-chan error) error {
	deadline := time.Now().Add(timeout)
	announced := false
	logPath := filepath.Join(home, daemonLogName)
	for {
		// Probe first: a concurrent CLI may have won the socket race while our
		// child exited with "address already in use".
		if err := probeDaemon(ctx, c); err == nil {
			return nil
		}
		if exited != nil {
			select {
			case err := <-exited:
				exited = nil
				// Two CLIs can race to start the daemon: the loser exits with
				// "address already in use" (or the home lock message) while the
				// winner is still binding. Keep probing instead of reporting the
				// loser as a failure.
				logTail := tailLog(home, 40)
				if strings.Contains(logTail, "address already in use") ||
					strings.Contains(logTail, "already serving") {
					break
				}
				if err == nil {
					err = errors.New("进程提前退出")
				}
				return fmt.Errorf("daemon 启动失败：%v；日志 %s：\n%s", err, logPath, tailLog(home, 20))
			default:
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon 启动超时（%s）；日志 %s：\n%s",
				timeout, logPath, tailLog(home, 20))
		}
		if !announced {
			announced = true
			fmt.Fprintln(os.Stderr, "正在启动 chyan 服务端…")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// tailLog returns the last n lines of the spawned daemon log.
func tailLog(home string, n int) string {
	raw, err := os.ReadFile(filepath.Join(home, daemonLogName))
	if err != nil {
		return "（无日志）"
	}
	// Diagnostics only need the tail; a rotated-but-large file must not be
	// copied whole into an error message.
	const tailBytes = 64 << 10
	if len(raw) > tailBytes {
		raw = raw[len(raw)-tailBytes:]
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			raw = raw[i+1:]
		}
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "  " + strings.Join(lines, "\n  ")
}
