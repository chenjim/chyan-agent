package memory

import (
	"strings"
	"unicode"
)

// tokens splits text into search tokens: lowercase ASCII words plus CJK
// bigrams (a lone CJK rune becomes a unigram). Query and index sides share
// this tokenizer so both sides agree (architecture 4.3 segmentation rule).
func tokens(text string) []string {
	var out []string
	var word []rune
	var cjk []rune
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, strings.ToLower(string(word)))
			word = word[:0]
		}
	}
	flushCJK := func() {
		if len(cjk) == 1 {
			out = append(out, string(cjk))
		}
		for i := 0; i+1 < len(cjk); i++ {
			out = append(out, string(cjk[i:i+2]))
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		case isWordRune(r):
			flushCJK()
			word = append(word, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return out
}

func isCJK(r rune) bool {
	return r > unicode.MaxASCII && unicode.IsLetter(r)
}

func isWordRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// tokenCounts builds a token multiset for occurrence scoring.
func tokenCounts(toks []string) map[string]int {
	m := make(map[string]int, len(toks))
	for _, t := range toks {
		m[t]++
	}
	return m
}

// hasTok reports whether tok is indexed in set. A single-rune token (e.g. a
// lone CJK char) also matches by substring, since the index only carries
// bigrams for multi-char CJK runs.
func hasTok(set map[string]int, lowerHay string, tok string) bool {
	if set[tok] > 0 {
		return true
	}
	for range tok { // exactly one rune
		return strings.Contains(lowerHay, tok)
	}
	return false
}

// overlap counts distinct query tokens present in one memory.
func overlap(set map[string]int, lowerHay string, queryTokens []string) int {
	seen := make(map[string]bool, len(queryTokens))
	n := 0
	for _, t := range queryTokens {
		if seen[t] {
			continue
		}
		seen[t] = true
		if hasTok(set, lowerHay, t) {
			n++
		}
	}
	return n
}
