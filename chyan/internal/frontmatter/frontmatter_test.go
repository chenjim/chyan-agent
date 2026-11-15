package frontmatter

import (
	"errors"
	"testing"
)

func TestSplit(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		fm      string // expected fm, "-" means nil
		body    string
		wantErr error
	}{
		{
			name: "valid block",
			in:   "---\nname: a\n---\nbody here\n",
			fm:   "name: a\n",
			body: "body here",
		},
		{
			name: "no frontmatter",
			in:   "just body\n",
			fm:   "-",
			body: "just body",
		},
		{
			name: "empty block",
			in:   "---\n---\nbody",
			fm:   "",
			body: "body",
		},
		{
			name: "crlf normalized",
			in:   "---\r\nname: a\r\n---\r\nbody\r\n",
			fm:   "name: a\n",
			body: "body",
		},
		{
			name: "utf-8 bom stripped",
			in:   "\xef\xbb\xbf---\nname: a\n---\nbody",
			fm:   "name: a\n",
			body: "body",
		},
		{
			name: "block ends at EOF",
			in:   "---\nname: a\n---",
			fm:   "name: a\n",
			body: "",
		},
		{
			name:    "unterminated",
			in:      "---\nname: a\nbody",
			wantErr: ErrUnterminated,
		},
		{
			name: "opening delim not at start",
			in:   "x\n---\nname: a\n---\nbody",
			fm:   "-",
			body: "x\n---\nname: a\n---\nbody",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm, body, err := Split([]byte(tt.in))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.fm == "-" {
				if fm != nil {
					t.Fatalf("fm = %q, want nil", fm)
				}
			} else if string(fm) != tt.fm {
				t.Fatalf("fm = %q, want %q", fm, tt.fm)
			}
			if body != tt.body {
				t.Fatalf("body = %q, want %q", body, tt.body)
			}
		})
	}
}
