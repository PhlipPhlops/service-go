package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A root agent-context file over this budget consumes context on every request
// and measurably reduces adherence; depth belongs in .claude/skills/ instead.
const agentContextMaxLines = 200

func TestAgentContextWithinLineBudget(t *testing.T) {
	data, err := os.ReadFile("AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(bytes.TrimRight(data, "\n"), []byte("\n")) + 1; lines > agentContextMaxLines {
		t.Errorf("AGENTS.md is %d lines, over the %d budget", lines, agentContextMaxLines)
	}
}

func TestClaudeFilePointsAtAgentContext(t *testing.T) {
	data, err := os.ReadFile("CLAUDE.md")
	if err != nil {
		t.Fatal(err)
	}
	// One canonical source: a second copy drifts from the first.
	if got := strings.TrimSpace(string(data)); got != "@AGENTS.md" {
		t.Errorf("CLAUDE.md must be the pointer %q, got %q", "@AGENTS.md", got)
	}
}

func TestSkillsDeclareTriggerableFrontmatter(t *testing.T) {
	paths, err := filepath.Glob(".claude/skills/*/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, rest, found := strings.Cut(string(data), "---\n")
		if !found {
			t.Errorf("%s: missing frontmatter", path)
			continue
		}
		front, _, found := strings.Cut(rest, "\n---")
		if !found {
			t.Errorf("%s: unterminated frontmatter", path)
			continue
		}
		var skill struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
		}
		if err := yaml.Unmarshal([]byte(front), &skill); err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if dir := filepath.Base(filepath.Dir(path)); skill.Name != dir {
			t.Errorf("%s: name %q must match its directory %q", path, skill.Name, dir)
		}
		// The description is the only thing an agent sees before loading the
		// skill, so an empty one makes the skill unreachable.
		if skill.Description == "" {
			t.Errorf("%s: description is required", path)
		}
		if len(skill.Description) > 1024 {
			t.Errorf("%s: description is %d chars, over the 1024 limit", path, len(skill.Description))
		}
	}
}
