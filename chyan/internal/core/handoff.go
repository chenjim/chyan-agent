package core

import (
	"context"
	"strings"
	"time"
)

func nowUTC() time.Time { return time.Now().UTC() }

// TruncateCompressor is the deterministic Compressor default: it trims the
// folded text to the target ratio of its original length. LLM-backed
// summarization is provided by the llm-backed implementation in A4 wiring.
type TruncateCompressor struct{}

// Compress implements Compressor: shorten msgs to roughly targetRatio of
// their flattened length, marked with an ellipsis.
func (TruncateCompressor) Compress(_ context.Context, msgs []Message, targetRatio float64) (string, error) {
	var b strings.Builder
	for _, msg := range msgs {
		for _, p := range msg.Content {
			if p.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(p.Text)
			}
		}
	}
	text := b.String()
	if targetRatio <= 0 {
		targetRatio = 0.3
	}
	// A ratio >= 1 asks for no reduction: return as-is.
	if targetRatio >= 1 {
		return text, nil
	}
	maxChars := int(float64(len([]rune(text))) * targetRatio)
	if maxChars < 1 {
		maxChars = 1
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text, nil
	}
	return string(runes[:maxChars]) + "…", nil
}

// HeuristicExtractor derives structured handoff sections without an LLM:
// first user message as Intent, marker-keyed lines into Done/Active/Next,
// referenced paths into References. Enough for continuity; the LLM extractor
// improves quality later.
type HeuristicExtractor struct{}

// Extract implements HandoffExtractor.
func (HeuristicExtractor) Extract(_ context.Context, msgs []Message) (HandoffNote, error) {
	var note HandoffNote
	note.GeneratedAt = nowUTC()
	for _, msg := range msgs {
		text := flattenText(msg)
		if text == "" {
			continue
		}
		if msg.Role == RoleUser && note.Intent == "" {
			note.Intent = firstLine(text)
		}
		for _, line := range strings.Split(text, "\n") {
			trimmed := strings.TrimSpace(line)
			body := ""
			if i := strings.Index(trimmed, ":"); i >= 0 {
				body = strings.TrimSpace(trimmed[i+1:])
			}
			switch {
			case hasPrefixFold(trimmed, "decision:"):
				note.Decisions = append(note.Decisions, body)
			case hasPrefixFold(trimmed, "correction:"), hasPrefixFold(trimmed, "corrected:"):
				note.Corrections = append(note.Corrections, body)
			case hasPrefixFold(trimmed, "done:"):
				note.Done = append(note.Done, body)
			case hasPrefixFold(trimmed, "todo:"), hasPrefixFold(trimmed, "next:"):
				note.Next = append(note.Next, body)
			case hasPrefixFold(trimmed, "active:"):
				note.Active = append(note.Active, body)
			case hasPrefixFold(trimmed, "lesson:"), hasPrefixFold(trimmed, "pitfall:"):
				note.Lessons = append(note.Lessons, body)
			}
			for _, ref := range extractRefs(trimmed) {
				note.References = append(note.References, ref)
			}
		}
	}
	return note, nil
}

// flattenText joins a message's text parts and tool names/inputs.
func flattenText(msg Message) string {
	var b strings.Builder
	for _, p := range msg.Content {
		if p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// hasPrefixFold matches prefix case-insensitively.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// extractRefs pulls path-like and URL references out of one line, capped so
// noisy output cannot flood the note.
func extractRefs(line string) []string {
	var refs []string
	for _, tok := range strings.Fields(line) {
		tok = strings.Trim(tok, "`\"'(),;:")
		if strings.HasPrefix(tok, "http://") || strings.HasPrefix(tok, "https://") {
			refs = append(refs, tok)
		} else if looksLikePath(tok) {
			refs = append(refs, tok)
		}
		if len(refs) >= 3 {
			break
		}
	}
	return refs
}

// looksLikePath is a cheap heuristic for file path tokens worth keeping.
// URLs are routed separately by extractRefs, so this stays path-only.
func looksLikePath(tok string) bool {
	if len(tok) < 3 || strings.Contains(tok, "://") || !strings.ContainsAny(tok, "/.") {
		return false
	}
	if strings.HasPrefix(tok, ".") && !strings.HasPrefix(tok, "./") {
		return false
	}
	slashes := strings.Count(tok, "/")
	if slashes >= 1 {
		return true
	}
	// Single-dot forms like "go.mod" or "main.go".
	return strings.Contains(tok, ".")
}
