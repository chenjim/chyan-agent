// System-prompt assembly: skills inventory then project directives.
package app

import (
	"strings"
	"testing"

	"chyan.tech/chyan/internal/skills"
)

func TestPromptWithSkillsAppendsDirectives(t *testing.T) {
	a := &App{directives: "\n\n## Project instructions\nfollow AGENTS.md"}
	if got, want := a.promptWithSkills("base prompt"), "base prompt"+a.directives; got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}

	a = &App{
		skills:     []skills.Skill{{Name: "pdf", Description: "read PDFs"}},
		directives: "\n\n## Project instructions\nrule",
	}
	got := a.promptWithSkills("base")
	skillAt := strings.Index(got, "## Available skills")
	dirAt := strings.Index(got, "## Project instructions")
	if skillAt < 0 || dirAt < 0 {
		t.Fatalf("prompt missing sections:\n%s", got)
	}
	if skillAt > dirAt {
		t.Fatalf("skills must precede directives:\n%s", got)
	}
	if !strings.Contains(got, "- pdf: read PDFs") {
		t.Fatalf("skill inventory missing:\n%s", got)
	}
}
