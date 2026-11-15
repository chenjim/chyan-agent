package memory

import (
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// meta is the YAML frontmatter of a memory markdown file (architecture 4.3).
type meta struct {
	Name       string   `yaml:"name"`
	Keywords   []string `yaml:"keywords"`
	Triggers   []string `yaml:"triggers"`
	Type       string   `yaml:"type"`
	Status     string   `yaml:"status"`
	Supersedes string   `yaml:"supersedes"`
	Source     string   `yaml:"source"`
	Pinned     bool     `yaml:"pinned"`
	ID         string   `yaml:"id"`
}

// renderFrontmatter serializes m as a frontmatter block with no body.
func renderFrontmatter(m *meta) string {
	var b strings.Builder
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		panic("memory: frontmatter encode: " + err.Error())
	}
	_ = enc.Close()
	b.WriteString("---\n")
	return b.String()
}

// slugMaxLen caps the <slug> file name part so paths stay short and readable.
const slugMaxLen = 24

// slugify derives the kebab-case <slug> of the file name <id>-<slug>.md from
// the memory name (architecture 4.3). The name is split on whitespace and a
// word is kept only when it is pure ASCII: mixed-script words such as
// "用户偏好Python" are dropped whole, so the slug abbreviates whole words of
// the name instead of harvesting keyword-like fragments out of them (that is
// what the keywords field is for). Kept words contribute their runs of ASCII
// letters/digits, lowercased and joined with "-". The result is capped at
// slugMaxLen characters and cut at a word boundary (a lone first word longer
// than the cap is cut mid-word). "" means the name has no usable word and
// callers substitute a fallback.
func slugify(name string) string {
	var words []string
	for _, w := range strings.Fields(name) {
		if !asciiOnly(w) {
			continue
		}
		words = append(words, strings.FieldsFunc(strings.ToLower(w), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})...)
	}
	var b strings.Builder
	for i, w := range words {
		if i == 0 {
			if len(w) > slugMaxLen {
				return w[:slugMaxLen]
			}
			b.WriteString(w)
			continue
		}
		if b.Len()+1+len(w) > slugMaxLen {
			break
		}
		b.WriteByte('-')
		b.WriteString(w)
	}
	return b.String()
}

// asciiOnly reports whether s contains no rune above ASCII.
func asciiOnly(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}
