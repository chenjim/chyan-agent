// Scheduling metrics: queue wait, turn duration and batch size are published
// as SSE events so slow work is visible without changing the agent path.
package daemon

import (
	"time"

	"chyan.tech/chyan/internal/api"
)

// publishMetric broadcasts one measurement. Data must be a fresh map: the
// timestamp is added in place.
func (s *Server) publishMetric(data map[string]any) {
	if s == nil || s.hub == nil {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	if _, ok := data["at"]; !ok {
		data["at"] = time.Now().UTC()
	}
	s.hub.publish(Event{Name: api.EventMetric, Data: data})
}
