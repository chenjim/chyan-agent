// Package frontmatter splits YAML frontmatter blocks out of markdown files.
package frontmatter

import (
	"bytes"
	"errors"
	"strings"
)

// ErrUnterminated reports a frontmatter block without a closing delimiter.
var ErrUnterminated = errors.New("frontmatter: unterminated block")

// Split returns the YAML bytes of the leading "---" delimited frontmatter
// block and the body after it. Contract:
//
//   - fm is the raw YAML text between the delimiters: it includes the newline
//     terminating the last YAML line, but never the "---" lines themselves nor
//     any blank line after the closing delimiter. A file without frontmatter
//     yields a nil fm block; an empty block yields empty (non-nil) fm bytes.
//   - body is the content after the block with surrounding whitespace trimmed;
//     without frontmatter the whole content is returned (trimmed as well).
//
// CRLF input is normalized to LF first, and a leading UTF-8 BOM is dropped:
// editors on Windows add both, and a BOM hides the "---" delimiter.
func Split(data []byte) (fm []byte, body string, err error) {
	content := bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(content, []byte("---\n")) {
		return nil, strings.TrimSpace(string(content)), nil
	}
	after := content[len("---\n"):]
	if bytes.HasPrefix(after, []byte("---\n")) {
		return []byte{}, strings.TrimSpace(string(after[len("---\n"):])), nil
	}
	if string(after) == "---" {
		return []byte{}, "", nil
	}
	if idx := bytes.Index(after, []byte("\n---\n")); idx >= 0 {
		return after[:idx+1], strings.TrimSpace(string(after[idx+len("\n---\n"):])), nil
	}
	if bytes.HasSuffix(after, []byte("\n---")) {
		return after[:len(after)-len("---")], "", nil
	}
	return nil, "", ErrUnterminated
}
