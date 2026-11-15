// tui.go — package entry: Run drives the bubbletea program against a daemon.
package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
)

// pollInterval is the periodic refresh for panels that have no event stream
// yet (session list, status counters).
const pollInterval = 3 * time.Second

// reconnectDelay is the pause before re-subscribing after an SSE drop. Fixed
// (not exponential): the daemon is usually local, and a steady retry keeps
// recovery quick without hammering a downed server.
const reconnectDelay = time.Second

// Run starts the TUI against the daemon at baseURL (Bearer token optional)
// and blocks until the user quits. It is the entry point used by the
// `chyan tui` subcommand.
func Run(ctx context.Context, baseURL, token string) error {
	client := NewClient(baseURL, token)
	events := make(chan Event, 64)

	// SSE subscription with reconnect: a dropped stream must not leave the
	// task panel frozen. Panels also poll (pollCmd), so state recovers even
	// while the stream is down.
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer close(events)
		for {
			_ = client.Events(subCtx, events)
			if subCtx.Err() != nil {
				return
			}
			select {
			case <-time.After(reconnectDelay):
			case <-subCtx.Done():
				return
			}
		}
	}()

	p := tea.NewProgram(newModel(ctx, client, events), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

// pollCmd waits one poll interval and reports back (msgPoll).
func pollCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return msgPoll{} })
}
