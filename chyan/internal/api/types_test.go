package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHandoffNoteJSONUsesSnakeCase keeps the wire shape consistent with the
// rest of the API (and with docs) instead of Go field names.
func TestHandoffNoteJSONUsesSnakeCase(t *testing.T) {
	raw, err := json.Marshal(CompactResult{Note: HandoffNote{
		Intent: "ship it", Decisions: []string{"use Go"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{`"intent":"ship it"`, `"decisions":["use Go"]`} {
		if !strings.Contains(got, want) {
			t.Errorf("json = %s, missing %s", got, want)
		}
	}
	if strings.Contains(got, `"Intent"`) {
		t.Errorf("json = %s, Go field names leaked", got)
	}
}
