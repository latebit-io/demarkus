package knowledgeseed

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

func TestPolicyBodyParsesPermissive(t *testing.T) {
	policy := publishpolicy.Parse(string(PolicyBody()))
	if err := policy.Validate(); err != nil {
		t.Fatalf("validate default policy: %v", err)
	}
	if policy.Strictness != publishpolicy.Warn {
		t.Errorf("strictness = %q, want %q", policy.Strictness, publishpolicy.Warn)
	}
	// Prose documents the tightening directives; a seeded world must not
	// inherit them, or its first publish fails on axes nobody chose.
	if len(policy.RequiredTagAxes) != 0 {
		t.Errorf("required tag axes = %v, want none", policy.RequiredTagAxes)
	}
	if len(policy.RequiredFields) != 0 {
		t.Errorf("required fields = %v, want none", policy.RequiredFields)
	}
}

func TestPolicyBodyFollowsStyleBaseline(t *testing.T) {
	body := string(PolicyBody())
	if !strings.HasPrefix(body, "# ") {
		t.Error("policy does not open with an H1 name")
	}
	if strings.HasPrefix(body, "---") {
		t.Error("policy opens with a frontmatter fence")
	}
	if strings.Contains(body, "—") {
		t.Error("policy contains an em dash")
	}
}

func TestAccessorsCopy(t *testing.T) {
	body := PolicyBody()
	body[0] = 'x'
	if PolicyBody()[0] != '#' {
		t.Error("PolicyBody exposed the embedded slice")
	}
	metadata := PolicyMetadata()
	metadata["tags"] = "mutated"
	if PolicyMetadata()["tags"] == "mutated" {
		t.Error("PolicyMetadata exposed the shared map")
	}
}
