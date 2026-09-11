package knowledgeseed

import (
	"strings"
	"testing"

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
