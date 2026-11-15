// mcpstatus_test.go — MCP server observability composed into GET /status (U2).
package daemon

import (
	"testing"
)

func TestMCPStatusesJoinsAvailabilityAndCounts(t *testing.T) {
	got := mcpStatuses(
		map[string]bool{"search": true, "playwright": false},
		// Counts come from the manager, so names sharing a prefix cannot
		// cross-count; exact counts keep them apart.
		map[string]int{"search": 2, "playwright": 1},
	)
	if len(got) != 2 {
		t.Fatalf("statuses = %+v", got)
	}
	// Sorted by name so the panel order is stable across polls.
	if got[0].Name != "playwright" || got[0].Available || got[0].Tools != 1 {
		t.Errorf("playwright = %+v", got[0])
	}
	if got[1].Name != "search" || !got[1].Available || got[1].Tools != 2 {
		t.Errorf("search = %+v", got[1])
	}
}

func TestMCPStatusesEmptyWithoutServers(t *testing.T) {
	if got := mcpStatuses(nil, map[string]int{"x": 1}); got != nil {
		t.Fatalf("statuses = %+v, want nil", got)
	}
	if got := mcpStatuses(map[string]bool{}, map[string]int{}); got != nil {
		t.Fatalf("statuses = %+v, want nil", got)
	}
}

func TestMCPStatusesMissingCountIsZero(t *testing.T) {
	// A server that connected but registered nothing (empty catalog) reports 0,
	// not a missing entry.
	got := mcpStatuses(map[string]bool{"empty": true}, nil)
	if len(got) != 1 || got[0].Tools != 0 {
		t.Fatalf("statuses = %+v", got)
	}
}
