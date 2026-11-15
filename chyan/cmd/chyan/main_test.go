package main

import (
	"bytes"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/core"
)

// TestRunOutputTextStreams: text format streams deltas live; JSON keeps stdout
// clean (nil sink).
func TestRunOutputTextStreams(t *testing.T) {
	var buf bytes.Buffer
	sink := runOutput(&buf, "text")
	if sink == nil {
		t.Fatal("text format must stream")
	}
	sink("hello ")
	sink("world")
	if buf.String() != "hello world" {
		t.Errorf("streamed = %q", buf.String())
	}

	var jbuf bytes.Buffer
	jsink := runOutput(&jbuf, "json")
	if jsink != nil {
		t.Error("json format must not stream to stdout")
	}
	if jbuf.String() != "" {
		t.Errorf("json stdout polluted: %q", jbuf.String())
	}
}

// TestFormatBudgetHumanReadable: "已用" is the next-call estimate (fixed cost
// + history, 4750 tok -> 4.8k / 0.5%), units are compact and both thresholds
// are visible (compression trigger + hard cap).
func TestFormatBudgetHumanReadable(t *testing.T) {
	out := formatBudget(core.Budget{
		ContextLimit: 1_000_000, FixedCost: 4699,
		HardLimit: 991_205, SoftLimit: 693_843,
		HistoryTokens: 51, HistoryBytes: 171,
	})
	for _, want := range []string{"1.00M", "4.8k", "0.5%", "历史 51 tok + 固定 4.7k", "693.8k", "69.4%", "991.2k", "99.1%", "171 B"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "context_limit=") {
		t.Error("old machine-readable format must be gone")
	}
}

func TestTokensAndByteSize(t *testing.T) {
	if got := tokensSize(1_000_000); got != "1.00M" {
		t.Errorf("tokensSize(1M) = %q", got)
	}
	if got := tokensSize(4699); got != "4.7k" {
		t.Errorf("tokensSize(4699) = %q", got)
	}
	if got := tokensSize(51); got != "51" {
		t.Errorf("tokensSize(51) = %q", got)
	}
	if got := byteSize(2 << 20); got != "2.0 MB" {
		t.Errorf("byteSize(2MB) = %q", got)
	}
	if got := byteSize(171); got != "171 B" {
		t.Errorf("byteSize(171) = %q", got)
	}
	if got := usageBar(0); len([]rune(got)) != 24 {
		t.Errorf("usageBar cells = %d", len([]rune(got)))
	}
}

// TestFormatHandoffAllSections: every HandoffNote section renders (the old
// output hid Done/Active/Next — the task list).
func TestFormatHandoffAllSections(t *testing.T) {
	out := formatHandoff(core.HandoffNote{
		Intent:      "do the thing",
		Corrections: []string{"not like that"},
		Decisions:   []string{"use sqlite"},
		Done:        []string{"built"},
		Active:      []string{"wiring"},
		Next:        []string{"ship it"},
		References:  []string{"/tmp/x"},
		Lessons:     []string{"check tests"},
		Summary:     "a summary",
	})
	for _, want := range []string{"do the thing", "not like that", "use sqlite",
		"built", "wiring", "ship it", "/tmp/x", "check tests", "a summary"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if got := formatHandoff(core.HandoffNote{Intent: "x"}); strings.Contains(got, "decisions:") {
		t.Errorf("empty sections must be skipped:\n%s", got)
	}
}
