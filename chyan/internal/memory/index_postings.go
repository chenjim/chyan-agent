package memory

import (
	"encoding/json"
	"os"
	"sort"
)

// Pure-Go inverted-index fallback of the memory index, used when the linked
// SQLite build has no FTS5 (docs/development-plan P4-3 fallback). State is a
// small JSON file rewritten atomically on every mutation: simple and
// reliable at the 1e2-1e4 document scale of the memory system.

// postingsBackend persists docs plus a token -> doc-key inverted index.
type postingsBackend struct {
	path     string
	Docs     map[string]*indexDoc `json:"docs"`     // docKey -> doc
	Postings map[string][]string  `json:"postings"` // token -> doc keys
}

func newPostingsBackend(path string) (backend, error) {
	if err := discardForeignCache(path, false); err != nil {
		return nil, err
	}
	b := &postingsBackend{
		path:     path,
		Docs:     map[string]*indexDoc{},
		Postings: map[string][]string{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return b, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, b); err != nil {
		// Corrupted cache: start fresh, Rebuild restores everything.
		b.Docs = map[string]*indexDoc{}
		b.Postings = map[string][]string{}
	}
	return b, nil
}

func (b *postingsBackend) upsert(docs []indexDoc) error {
	for _, d := range docs {
		d := d
		key := docKey(d)
		b.removeKeyLocked(key)
		b.Docs[key] = &d
		for _, tok := range dedup(indexTokens(docText(d))) {
			b.Postings[tok] = append(b.Postings[tok], key)
		}
	}
	return b.save()
}

func (b *postingsBackend) remove(ids []string) error {
	// Ids are only unique per domain, so drop every row sharing the id.
	var keys []string
	for key, d := range b.Docs {
		for _, id := range ids {
			if d.ID == id {
				keys = append(keys, key)
				break
			}
		}
	}
	for _, key := range keys {
		b.removeKeyLocked(key)
	}
	return b.save()
}

func (b *postingsBackend) replaceUnder(root string, docs []indexDoc) error {
	for key, d := range b.Docs {
		if underRoot(d.Path, root) || (d.Path == "" && underRoot(key, root)) {
			b.removeKeyLocked(key)
		}
	}
	return b.upsert(docs)
}

// match scores docs by term frequency over the query tokens (the postings
// counterpart of bm25 at small scale). Semantics are AND, matching FTS5's
// implicit AND: a doc must contain every distinct query token, otherwise one
// shared common word drags in unrelated memories.
func (b *postingsBackend) match(queryTokens []string) ([]matchResult, error) {
	tokens := dedup(append([]string(nil), queryTokens...)) // dedup rewrites in place
	if len(tokens) == 0 {
		return nil, nil
	}
	// Count how many distinct tokens each candidate doc covers.
	hits := map[string]int{}
	for _, key := range b.Postings[tokens[0]] {
		hits[key] = 1
	}
	if len(hits) == 0 {
		return nil, nil
	}
	for _, tok := range tokens[1:] {
		keys := b.Postings[tok]
		if len(keys) == 0 {
			return nil, nil // no doc can cover all tokens
		}
		seen := map[string]bool{}
		for _, key := range keys {
			if _, ok := hits[key]; ok && !seen[key] {
				hits[key]++
				seen[key] = true
			}
		}
	}
	out := make([]matchResult, 0, len(hits))
	for key, n := range hits {
		if n != len(tokens) {
			continue
		}
		d := b.Docs[key]
		if d == nil {
			continue
		}
		var rel float64
		for _, tok := range tokens {
			rel += float64(tokenFreq(d, tok))
		}
		out = append(out, matchResult{doc: *d, rel: rel})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].doc.ID < out[j].doc.ID })
	return out, nil
}

func (b *postingsBackend) close() error {
	return nil
}

// removeKeyLocked drops one doc and its postings entries.
func (b *postingsBackend) removeKeyLocked(key string) {
	d := b.Docs[key]
	if d == nil {
		return
	}
	for _, tok := range dedup(indexTokens(docText(*d))) {
		list := b.Postings[tok]
		out := list[:0]
		for _, k := range list {
			if k != key {
				out = append(out, k)
			}
		}
		if len(out) == 0 {
			delete(b.Postings, tok)
		} else {
			b.Postings[tok] = out
		}
	}
	delete(b.Docs, key)
}

// save rewrites the JSON cache atomically (tmp file + rename).
func (b *postingsBackend) save() error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}

// tokenFreq counts token occurrences in a doc (term frequency for scoring).
func tokenFreq(d *indexDoc, tok string) int {
	if d == nil {
		return 0
	}
	n := 0
	for _, t := range indexTokens(docText(*d)) {
		if t == tok {
			n++
		}
	}
	return n
}

// MarshalJSON keeps the on-disk cache format explicit and versioned.
func (b *postingsBackend) MarshalJSON() ([]byte, error) {
	type wire struct {
		Docs     map[string]*indexDoc `json:"docs"`
		Postings map[string][]string  `json:"postings"`
		Version  int                  `json:"version"`
	}
	return json.Marshal(wire{Docs: b.Docs, Postings: b.Postings, Version: 1})
}
