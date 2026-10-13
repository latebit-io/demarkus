package writepolicy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

const warnPolicy = "# Write Policy\n\nstrictness: warn\n"

func fixedFloor(policy publishpolicy.Policy) func(context.Context) (publishpolicy.Policy, error) {
	return func(context.Context) (publishpolicy.Policy, error) { return policy, nil }
}

func TestEnforceFloor(t *testing.T) {
	raw := newStore(t)
	if _, err := (backendtest.Direct{Store: raw}).WriteVersion(publishpolicy.DocumentPath, 0, []byte(warnPolicy), policyMeta); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	floor := publishpolicy.Policy{Strictness: publishpolicy.Block, RequiredTagAxes: []string{"category"},
		AllowedTagValues: map[string][]string{"category": {"guide", "decision"}}}
	gated := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true, Floor: fixedFloor(floor)})}

	// The world says warn; the floor says block and requires an axis.
	_, err := gated.WriteVersion("/a.md", 0, []byte("# A\n"), map[string]string{"tags": "topic"})
	var policyErr *writepolicy.PolicyError
	if !errors.Is(err, writepolicy.ErrPolicyBlocked) || !errors.As(err, &policyErr) || policyErr.Strictness != publishpolicy.Block {
		t.Fatalf("write under the floor = %v, want blocked at the floor's strictness", err)
	}
	if _, err := gated.WriteVersion("/a.md", 0, []byte("# A\n"), map[string]string{"tags": "category:howto"}); !errors.Is(err, writepolicy.ErrPolicyBlocked) {
		t.Errorf("disallowed value = %v, want blocked", err)
	} else if !strings.Contains(policyErrOf(t, err).RejectionMessage(), "disallowed-tag-value: category:howto (allowed category values: guide|decision)") {
		t.Errorf("rejection = %q, want the disallowed tag named", policyErrOf(t, err).RejectionMessage())
	}
	if _, err := gated.WriteVersion("/a.md", 0, []byte("# A\n"), map[string]string{"tags": "category:guide"}); err != nil {
		t.Errorf("compliant write: %v", err)
	}

	// A world policy disjoint from the floor on an axis would refuse every
	// value on it; it is refused instead.
	disjoint := "# Write Policy\n\nstrictness: warn\nallow_tags.category: howto\n"
	if _, err := gated.WriteVersion(publishpolicy.DocumentPath, 1, []byte(disjoint), policyMeta); !errors.Is(err, writepolicy.ErrInvalidPolicy) {
		t.Errorf("world policy disjoint from the floor = %v, want ErrInvalidPolicy", err)
	}
	tighter := "# Write Policy\n\nstrictness: block\nallow_tags.category: guide\n"
	if _, err := gated.WriteVersion(publishpolicy.DocumentPath, 1, []byte(tighter), map[string]string{"tags": "category:guide"}); err != nil {
		t.Errorf("world policy tightening the floor: %v", err)
	}

	// A floor that cannot be read refuses the write rather than skipping it.
	broken := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true,
		Floor: func(context.Context) (publishpolicy.Policy, error) {
			return publishpolicy.Policy{}, errors.New("root world not open")
		}})}
	if _, err := broken.WriteVersion("/b.md", 0, []byte("# B\n"), map[string]string{"tags": "category:guide"}); !errors.Is(err, writepolicy.ErrInvalidPolicy) {
		t.Errorf("unreadable floor = %v, want ErrInvalidPolicy", err)
	}
}

func policyErrOf(t *testing.T, err error) *writepolicy.PolicyError {
	t.Helper()
	var policyErr *writepolicy.PolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("%v is not a PolicyError", err)
	}
	return policyErr
}

func TestEnforceFloorCandidateCheckedAgainstWorlds(t *testing.T) {
	raw := newStore(t)
	if _, err := (backendtest.Direct{Store: raw}).WriteVersion(publishpolicy.DocumentPath, 0, []byte(warnPolicy), policyMeta); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	world := publishpolicy.Policy{AllowedTagValues: map[string][]string{"category": {"guide"}}}
	gated := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true,
		Worlds: func(context.Context) ([]publishpolicy.Policy, error) { return []publishpolicy.Policy{world}, nil }})}
	// Disjoint allowed lists would leave the world unable to tag the axis.
	disjoint := "# Write Policy\n\nstrictness: block\nallow_tags.category: decision\n"
	if _, err := gated.WriteVersion(publishpolicy.DocumentPath, 1, []byte(disjoint), policyMeta); !errors.Is(err, writepolicy.ErrInvalidPolicy) {
		t.Errorf("unsatisfiable floor candidate = %v, want ErrInvalidPolicy", err)
	}
	compatible := "# Write Policy\n\nstrictness: block\nallow_tags.category: guide|decision\n"
	if _, err := gated.WriteVersion(publishpolicy.DocumentPath, 1, []byte(compatible), policyMeta); err != nil {
		t.Errorf("compatible floor candidate: %v", err)
	}
	unreadable := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true,
		Worlds: func(context.Context) ([]publishpolicy.Policy, error) { return nil, errors.New("a world is closed") }})}
	if _, err := unreadable.WriteVersion(publishpolicy.DocumentPath, 2, []byte(compatible+"\nRevised.\n"), policyMeta); !errors.Is(err, writepolicy.ErrInvalidPolicy) {
		t.Errorf("floor candidate with unreadable worlds = %v, want ErrInvalidPolicy", err)
	}
}

func TestEnforceContent(t *testing.T) {
	raw := newStore(t)
	if _, err := (backendtest.Direct{Store: raw}).WriteVersion(publishpolicy.DocumentPath, 0, []byte(warnPolicy), policyMeta); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	type warning struct {
		path     string
		rules    []string
		override string
	}
	var warned []warning
	warn := func(path string, findings []contentcheck.Finding, override string) {
		w := warning{path: path, override: override}
		for _, f := range findings {
			w.rules = append(w.rules, f.Rule)
		}
		warned = append(warned, w)
	}
	tags := map[string]string{"tags": "topic"}
	secret := []byte("# Keys\n\n```\n-----BEGIN RSA PRIVATE KEY-----\n```\n")
	sin := []byte("# Person\n\nSIN 046 454 286\n")

	t.Run("secrets block under every mode", func(t *testing.T) {
		for _, mode := range []contentcheck.Mode{contentcheck.Block, contentcheck.Warn, contentcheck.Off} {
			gated := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true, Content: writepolicy.ContentOptions{Options: contentcheck.Options{PII: mode}}})}
			_, err := gated.WriteVersion("/keys.md", 0, secret, tags)
			var contentErr *writepolicy.ContentError
			if !errors.Is(err, writepolicy.ErrContentBlocked) || !errors.Is(err, backend.ErrRejected) || !errors.As(err, &contentErr) {
				t.Fatalf("mode %s: secret = %v, want ErrContentBlocked", mode, err)
			}
			msg := contentErr.RejectionMessage()
			if !strings.Contains(msg, "secret/pem-private-key at line 4") || strings.Contains(msg, "RSA") {
				t.Errorf("rejection = %q, want the rule and line, never the value", msg)
			}
		}
	})
	t.Run("pii follows the mode", func(t *testing.T) {
		block := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true, Content: writepolicy.ContentOptions{Warn: warn}})}
		if _, err := block.WriteVersion("/p.md", 0, sin, tags); !errors.Is(err, writepolicy.ErrContentBlocked) {
			t.Errorf("default mode pii = %v, want blocked", err)
		}
		warned = nil
		warnMode := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true, Content: writepolicy.ContentOptions{Options: contentcheck.Options{PII: contentcheck.Warn}, Warn: warn}})}
		if _, err := warnMode.WriteVersion("/p.md", 0, sin, tags); err != nil {
			t.Fatalf("warn mode pii: %v", err)
		}
		if len(warned) != 1 || warned[0].path != "/p.md" || warned[0].override != "" || strings.Join(warned[0].rules, ",") != "canadian-sin" {
			t.Errorf("warned = %+v", warned)
		}
		off := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true, Content: writepolicy.ContentOptions{Options: contentcheck.Options{PII: contentcheck.Off}, Warn: warn}})}
		warned = nil
		if _, err := off.WriteVersion("/q.md", 0, sin, tags); err != nil || len(warned) != 0 {
			t.Errorf("off mode pii = %v, warned %+v", err, warned)
		}
	})
	t.Run("override lands personal data and is reported, never a secret", func(t *testing.T) {
		warned = nil
		gated := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true, Content: writepolicy.ContentOptions{Warn: warn}})}
		meta := map[string]string{"tags": "topic", contentcheck.OverrideKey: "test fixture number"}
		doc, err := gated.WriteVersion("/sin.md", 0, sin, meta)
		if err != nil {
			t.Fatalf("override: %v", err)
		}
		if doc.Metadata[contentcheck.OverrideKey] != "test fixture number" {
			t.Errorf("override not recorded: %v", doc.Metadata)
		}
		if len(warned) != 1 || warned[0].override != "test fixture number" {
			t.Errorf("warned = %+v", warned)
		}
		if _, err := gated.WriteVersion("/sin.md", 1, sin, map[string]string{"tags": "topic", contentcheck.OverrideKey: "  "}); !errors.Is(err, writepolicy.ErrContentBlocked) {
			t.Errorf("blank override = %v, want blocked", err)
		}
		_, err = gated.WriteVersion("/keys.md", 0, secret, map[string]string{"tags": "topic", contentcheck.OverrideKey: "documented example key"})
		var contentErr *writepolicy.ContentError
		if !errors.Is(err, writepolicy.ErrContentBlocked) || !errors.As(err, &contentErr) || !strings.Contains(contentErr.RejectionMessage(), contentcheck.Remedy(contentErr.Findings)) {
			t.Errorf("secret under override = %v, want blocked with no override offered", err)
		}
	})
	t.Run("the scan runs before the policy and before the floor is read", func(t *testing.T) {
		gated := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true, Floor: fixedFloor(publishpolicy.Policy{Strictness: publishpolicy.Block, RequiredTagAxes: []string{"category"}})})}
		if _, err := gated.WriteVersion("/r.md", 0, secret, nil); !errors.Is(err, writepolicy.ErrContentBlocked) {
			t.Errorf("secret with bad metadata = %v, want the content refusal first", err)
		}
		unreadable := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true,
			Floor: func(context.Context) (publishpolicy.Policy, error) {
				return publishpolicy.Policy{}, errors.New("floor down")
			}})}
		if _, err := unreadable.WriteVersion("/r.md", 0, secret, tags); !errors.Is(err, writepolicy.ErrContentBlocked) {
			t.Errorf("secret with the floor unreadable = %v, want the content refusal, not the floor error", err)
		}
	})
}
