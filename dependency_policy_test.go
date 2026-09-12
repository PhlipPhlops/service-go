package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func readPolicyYAML(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, target); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestDependencyUpdatesGroupEveryEcosystem(t *testing.T) {
	var config struct {
		Updates []struct {
			Ecosystem string `yaml:"package-ecosystem"`
			Directory string `yaml:"directory"`
			Limit     int    `yaml:"open-pull-requests-limit"`
			Groups    map[string]struct {
				Patterns []string `yaml:"patterns"`
				Types    []string `yaml:"update-types"`
			} `yaml:"groups"`
		} `yaml:"updates"`
	}
	readPolicyYAML(t, ".github/dependabot.yml", &config)
	wantDirectories := map[string]string{"gomod": "/", "docker": "/templates/builder", "github-actions": "/"}
	for _, update := range config.Updates {
		if dir, ok := wantDirectories[update.Ecosystem]; !ok || dir != update.Directory {
			t.Errorf("unexpected update location: %s %s", update.Ecosystem, update.Directory)
		}
		delete(wantDirectories, update.Ecosystem)
		if update.Limit != 1 {
			t.Errorf("%s: a week must yield one pull request, not %d", update.Ecosystem, update.Limit)
		}
		if len(update.Groups) != 1 {
			t.Errorf("%s: every dependency belongs to one group, found %d", update.Ecosystem, len(update.Groups))
		}
		for name, group := range update.Groups {
			if !slices.Equal(group.Patterns, []string{"*"}) {
				t.Errorf("%s/%s must cover all dependencies", update.Ecosystem, name)
			}
			// `update-types` only matches semver bumps, so a digest-pinned
			// image would fall out of the group into a pull request of its own.
			if len(group.Types) != 0 {
				t.Errorf("%s/%s: update-types drops digest updates out of the group: %v", update.Ecosystem, name, group.Types)
			}
		}
	}
	if len(wantDirectories) != 0 {
		t.Errorf("missing update ecosystems: %v", wantDirectories)
	}
}

func TestWorkflowReferencesAreImmutable(t *testing.T) {
	paths, err := filepath.Glob(".github/workflows/*")
	if err != nil {
		t.Fatal(err)
	}
	immutable := regexp.MustCompile(`@[0-9a-f]{40}$`)
	var check func(string, yaml.Node)
	check = func(path string, node yaml.Node) {
		if node.Kind == yaml.MappingNode {
			for i := 0; i < len(node.Content); i += 2 {
				key, value := node.Content[i], node.Content[i+1]
				if key.Value == "uses" && !strings.HasPrefix(value.Value, "./") && !immutable.MatchString(value.Value) {
					t.Errorf("%s:%d: mutable workflow/action reference %q", path, value.Line, value.Value)
				}
			}
		}
		for _, child := range node.Content {
			check(path, *child)
		}
	}
	for _, path := range paths {
		if ext := filepath.Ext(path); ext != ".yml" && ext != ".yaml" {
			continue
		}
		var workflow yaml.Node
		readPolicyYAML(t, path, &workflow)
		check(path, workflow)
	}
}

func TestPullRequestsRunTests(t *testing.T) {
	var workflow struct {
		On   map[string]any `yaml:"on"`
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	readPolicyYAML(t, ".github/workflows/ci.yml", &workflow)
	if _, ok := workflow.On["pull_request"]; !ok {
		t.Fatal("CI must run on pull requests")
	}
	for _, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if strings.TrimSpace(step.Run) == "go test ./..." {
				return
			}
		}
	}
	t.Fatal("PR CI must run the Go test suite before dependency updates merge")
}
