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

func TestDependencyUpdatesIsolateMajors(t *testing.T) {
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
		if update.Limit < 2 {
			t.Errorf("%s: a pending major must leave room for routine updates", update.Ecosystem)
		}
		owners := map[string]string{}
		for name, group := range update.Groups {
			if !slices.Equal(group.Patterns, []string{"*"}) {
				t.Errorf("%s/%s must cover all dependencies", update.Ecosystem, name)
			}
			for _, kind := range group.Types {
				if previous := owners[kind]; previous != "" {
					t.Errorf("%s: %s overlaps groups %s and %s", update.Ecosystem, kind, previous, name)
				}
				owners[kind] = name
			}
		}
		if owners["major"] == "" || owners["minor"] == "" || owners["patch"] == "" ||
			owners["major"] == owners["minor"] || owners["major"] == owners["patch"] {
			t.Errorf("%s: major updates must be covered separately from minor/patch: %v", update.Ecosystem, owners)
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
