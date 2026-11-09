package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A hand edit to any generated output must schedule the job that runs
// `plugin-prompts check`, so every output sits under an agent_plugins glob.
func TestCIAgentPluginsFilterCoversEveryOutput(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	globs := ciFilter(t, filepath.Join(root, ".github", "workflows", "ci.yml"), "agent_plugins")
	for target, out := range canonicalOutputs {
		covers := func(glob string) bool {
			prefix, ok := strings.CutSuffix(glob, "**")
			return ok && strings.HasPrefix(out+"/", prefix)
		}
		if !slices.ContainsFunc(globs, covers) {
			t.Errorf("%s output %s is not under any agent_plugins filter glob", target, out)
		}
	}
}

// ciFilter returns one paths-filter list from the detect-changes job.
func ciFilter(t *testing.T, path, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, step := range workflow.Jobs["detect-changes"].Steps {
		block, ok := step.With["filters"]
		if !ok {
			continue
		}
		var filters map[string][]string
		if err := yaml.Unmarshal([]byte(block), &filters); err != nil {
			t.Fatalf("parse filters block: %v", err)
		}
		if globs, ok := filters[name]; ok {
			return globs
		}
	}
	t.Fatalf("no %s filter in the detect-changes job of %s", name, path)
	return nil
}
