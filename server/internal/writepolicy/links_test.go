package writepolicy_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

// allow_domains holds body links: outside it, a block policy refuses and
// names the host; inside it, the write lands.
func TestEnforceAllowDomains(t *testing.T) {
	raw := newStore(t)
	policy := "# Write Policy\n\nstrictness: block\nallow_domains: github.com\n"
	if _, err := (backendtest.Direct{Store: raw}).WriteVersion(publishpolicy.DocumentPath, 0, []byte(policy), policyMeta); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	gated := writepolicy.Enforce(raw, writepolicy.Options{Require: true})
	tags := map[string]string{"tags": "topic"}
	_, err := write(t, gated, "/x.md", 0, []byte("# X\n\n![p](https://evil.io/p.png)\n"), tags)
	var refused *writepolicy.PolicyError
	if !errors.As(err, &refused) || !strings.Contains(refused.RejectionMessage(), "disallowed-link-domain: evil.io (allow_domains: github.com)") {
		t.Fatalf("outside link = %v", err)
	}
	if _, err := write(t, gated, "/y.md", 0, []byte("# Y\n\n[repo](https://docs.github.com/x)\n"), tags); err != nil {
		t.Errorf("allowed link refused: %v", err)
	}
}

// A world list disjoint from the floor's would refuse every link; the
// candidate policy is refused instead.
func TestCandidateDomainsMustMeetTheFloor(t *testing.T) {
	raw := newStore(t)
	if _, err := (backendtest.Direct{Store: raw}).WriteVersion(publishpolicy.DocumentPath, 0, []byte(warnPolicy), policyMeta); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	gated := writepolicy.Enforce(raw, writepolicy.Options{Require: true, Floor: fixedFloor(publishpolicy.Policy{AllowDomains: []string{"a.io"}})})
	_, err := write(t, gated, publishpolicy.DocumentPath, 1, []byte("# Write Policy\n\nstrictness: warn\nallow_domains: b.io\n"), policyMeta)
	if !errors.Is(err, writepolicy.ErrInvalidPolicy) || !strings.Contains(err.Error(), "allow_domains allows no domains") {
		t.Errorf("disjoint candidate = %v", err)
	}
}
