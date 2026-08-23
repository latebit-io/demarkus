package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/lockdir"
)

func setupConfigHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DEMARKUS_KNOWLEDGE_STRICTNESS", "")
	dir := filepath.Join(home, ".demarkus")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestKnowledgePolicy(t *testing.T) {
	t.Run("defaults to warn", func(t *testing.T) {
		setupConfigHome(t, nil)
		policy, err := KnowledgePolicy("knowledge")
		if err != nil {
			t.Fatal(err)
		}
		if policy.Strictness != publishpolicy.Warn {
			t.Fatalf("strictness = %q, want warn", policy.Strictness)
		}
	})

	t.Run("loads all mirrored fields", func(t *testing.T) {
		setupConfigHome(t, map[string]string{
			"plugin-knowledge.strictness.knowledge":     "block\n",
			"plugin-knowledge.require-tags.knowledge":   "category, team\nowner\n",
			"plugin-knowledge.require-fields.knowledge": "type authors\n",
		})
		policy, err := KnowledgePolicy("knowledge")
		if err != nil {
			t.Fatal(err)
		}
		want := publishpolicy.Policy{
			Strictness:      publishpolicy.Block,
			RequiredTagAxes: []string{"category", "team", "owner"},
			RequiredFields:  []string{"type", "authors"},
		}
		if !reflect.DeepEqual(policy, want) {
			t.Fatalf("KnowledgePolicy() = %#v, want %#v", policy, want)
		}
	})

	t.Run("atomic snapshot takes precedence", func(t *testing.T) {
		setupConfigHome(t, map[string]string{
			"plugin-knowledge.policy.knowledge":         "# mirror\nstrictness: block\nrequire_tags: category team\nrequire_fields: type\n",
			"plugin-knowledge.strictness.knowledge":     "warn\n",
			"plugin-knowledge.require-tags.knowledge":   "stale\n",
			"plugin-knowledge.require-fields.knowledge": "owner\n",
		})
		policy, err := KnowledgePolicy("knowledge")
		if err != nil {
			t.Fatal(err)
		}
		want := publishpolicy.Policy{
			Strictness:      publishpolicy.Block,
			RequiredTagAxes: []string{"category", "team"},
			RequiredFields:  []string{"type"},
		}
		if !reflect.DeepEqual(policy, want) {
			t.Fatalf("KnowledgePolicy() = %#v, want %#v", policy, want)
		}
	})

	t.Run("environment overrides file", func(t *testing.T) {
		setupConfigHome(t, map[string]string{
			"plugin-knowledge.strictness.knowledge": "block\n",
		})
		t.Setenv("DEMARKUS_KNOWLEDGE_STRICTNESS", " ask ")
		policy, err := KnowledgePolicy("knowledge")
		if err != nil {
			t.Fatal(err)
		}
		if policy.Strictness != publishpolicy.Ask {
			t.Fatalf("strictness = %q, want ask", policy.Strictness)
		}
	})

	t.Run("invalid strictness defaults to warn", func(t *testing.T) {
		setupConfigHome(t, map[string]string{
			"plugin-knowledge.strictness.knowledge": "invalid\n",
		})
		policy, err := KnowledgePolicy("knowledge")
		if err != nil {
			t.Fatal(err)
		}
		if policy.Strictness != publishpolicy.Warn {
			t.Fatalf("strictness = %q, want warn", policy.Strictness)
		}
	})

	t.Run("I/O errors propagate", func(t *testing.T) {
		dir := setupConfigHome(t, nil)
		if err := os.Mkdir(filepath.Join(dir, "plugin-knowledge.require-fields.knowledge"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := KnowledgePolicy("knowledge"); err == nil {
			t.Fatal("KnowledgePolicy succeeded with unreadable required-fields path")
		}
	})
}

func TestKnowledgePolicyFallbackWaitsForMirror(t *testing.T) {
	dir := setupConfigHome(t, map[string]string{
		"plugin-knowledge.strictness.knowledge":   "warn\n",
		"plugin-knowledge.require-tags.knowledge": "stale\n",
	})
	locked := make(chan struct{})
	release := make(chan struct{})
	lockErr := make(chan error, 1)
	go func() {
		lockErr <- lockdir.WithLock(filepath.Join(dir, "knowledge-systems.lock"), 1, time.Millisecond, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	type result struct {
		policy publishpolicy.Policy
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		policy, err := KnowledgePolicy("knowledge")
		resultCh <- result{policy: policy, err: err}
	}()
	select {
	case result := <-resultCh:
		t.Fatalf("policy read bypassed mirror lock: %#v, %v", result.policy, result.err)
	case <-time.After(25 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin-knowledge.policy.knowledge"), []byte("# mirror\nstrictness: block\nrequire_tags: category\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-lockErr; err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatal(result.err)
		}
		want := publishpolicy.Policy{Strictness: publishpolicy.Block, RequiredTagAxes: []string{"category"}}
		if !reflect.DeepEqual(result.policy, want) {
			t.Fatalf("KnowledgePolicy() = %#v, want %#v", result.policy, want)
		}
	case <-time.After(time.Second):
		t.Fatal("policy read did not complete after mirror lock released")
	}
}
