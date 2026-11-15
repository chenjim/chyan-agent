// Scheduling metric tests: queue wait, turn duration and batch size reach SSE.
package daemon

import (
	"testing"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/channels"
)

// waitMetric reads hub events until the metric of the given kind arrives.
func waitMetric(t *testing.T, events <-chan Event, kind string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("hub closed before the %s metric arrived", kind)
			}
			if ev.Name != api.EventMetric {
				continue
			}
			data, ok := ev.Data.(map[string]any)
			if !ok || data["kind"] != kind {
				continue
			}
			return data
		case <-deadline:
			t.Fatalf("no %s metric event", kind)
			return nil
		}
	}
}

func TestInboundTurnMetric(t *testing.T) {
	s, ch := newInboundHarness(t, &stubBackend{reply: "agent reply"}, channels.InboundPolicy{
		Enabled: true, AllowedChats: []string{"oc_m"}, MinInterval: time.Nanosecond,
	})
	events, unsub := s.hub.subscribe()
	defer unsub()

	ch.updatesC <- channels.Message{ID: "m1", ChatID: "oc_m", From: "u1", Text: "work"}
	ch.waitReplyMatch(t, func(v string) bool { return v == "agent reply" })

	m := waitMetric(t, events, "im.turn")
	if m["channel"] != "feishu" || m["chat_id"] != "oc_m" {
		t.Fatalf("metric target = %v", m)
	}
	if batch, _ := m["batch"].(int); batch < 1 {
		t.Fatalf("batch = %v", m["batch"])
	}
	if q, _ := m["queued_ms"].(int64); q < 0 {
		t.Fatalf("queued_ms = %v", m["queued_ms"])
	}
	if ok, _ := m["ok"].(bool); !ok {
		t.Fatalf("ok = %v", m["ok"])
	}
}

func TestTaskTurnMetric(t *testing.T) {
	s, _ := newStabilityServer(t, &stubBackend{reply: "done"}, 2, time.Minute)
	events, unsub := s.hub.subscribe()
	defer unsub()

	task := createTask(t, s.Handler())
	m := waitMetric(t, events, "task.turn")
	if m["task_id"] != task.ID {
		t.Fatalf("metric task_id = %v, want %s", m["task_id"], task.ID)
	}
	if ok, _ := m["ok"].(bool); !ok {
		t.Fatalf("task metric ok = %v", m["ok"])
	}
}
