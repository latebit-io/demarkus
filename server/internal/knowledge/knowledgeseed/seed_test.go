package knowledgeseed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

func TestDefaultPolicyParsesPermissive(t *testing.T) {
	policy := publishpolicy.Parse(string(DefaultPolicySeed().Body))
	if err := policy.Validate(); err != nil {
		t.Fatalf("validate default policy: %v", err)
	}
	if policy.Strictness != publishpolicy.Warn {
		t.Errorf("strictness = %q, want %q", policy.Strictness, publishpolicy.Warn)
	}
	// A seeded world must not inherit axes nobody chose, so prose naming a
	// directive must never start the line it sits on.
	if len(policy.RequiredTagAxes) != 0 {
		t.Errorf("required tag axes = %v, want none", policy.RequiredTagAxes)
	}
	if len(policy.RequiredFields) != 0 {
		t.Errorf("required fields = %v, want none", policy.RequiredFields)
	}
}

func TestDefaultPolicyFollowsStyleBaseline(t *testing.T) {
	body := string(DefaultPolicySeed().Body)
	if !strings.HasPrefix(body, "# ") {
		t.Error("policy does not open with an H1 name")
	}
	if strings.Contains(body, "—") {
		t.Error("policy contains an em dash")
	}
}

func TestDefaultPolicySeedIsFresh(t *testing.T) {
	seed := DefaultPolicySeed()
	seed.Body[0] = 'x'
	seed.Metadata["tags"] = "mutated"
	fresh := DefaultPolicySeed()
	if fresh.Body[0] != '#' || fresh.Metadata["tags"] == "mutated" {
		t.Error("DefaultPolicySeed shares state between calls")
	}
}

func TestPolicySeedFromFile(t *testing.T) {
	t.Run("carries the file body and the shared metadata", func(t *testing.T) {
		body := "# Write Policy\n\nOurs.\n\nstrictness: block\nrequire_tags: category\n"
		seed, err := PolicySeedFromFile(writePolicy(t, body))
		if err != nil {
			t.Fatalf("PolicySeedFromFile: %v", err)
		}
		if string(seed.Body) != body {
			t.Errorf("body = %q", seed.Body)
		}
		for key, expected := range DefaultPolicySeed().Metadata {
			if seed.Metadata[key] != expected {
				t.Errorf("metadata[%q] = %q, want %q", key, seed.Metadata[key], expected)
			}
		}
	})

	t.Run("rejects a policy that would not survive a publish", func(t *testing.T) {
		if _, err := PolicySeedFromFile(writePolicy(t, "strictness: nonsense\n")); err == nil {
			t.Fatal("accepted an unenforceable policy")
		}
	})

	t.Run("rejects a missing file", func(t *testing.T) {
		if _, err := PolicySeedFromFile(filepath.Join(t.TempDir(), "absent.md")); err == nil {
			t.Fatal("accepted a missing file")
		}
	})

	t.Run("rejects a directory", func(t *testing.T) {
		if _, err := PolicySeedFromFile(t.TempDir()); err == nil {
			t.Fatal("accepted a directory")
		}
	})

	t.Run("rejects a body past the protocol limit", func(t *testing.T) {
		oversize := "strictness: warn\n" + strings.Repeat("x", protocol.MaxBodyLength)
		_, err := PolicySeedFromFile(writePolicy(t, oversize))
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversize error = %v", err)
		}
	})
}

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "policy.md")
	if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return name
}
