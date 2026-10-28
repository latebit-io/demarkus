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
	"github.com/latebit-io/demarkus/server/internal/storetest"
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
		block := writepolicy.Enforce(raw, writepolicy.Options{Require: true})
		if _, err := write(t, block, "/p.md", 0, sin, tags); !errors.Is(err, writepolicy.ErrContentBlocked) {
			t.Errorf("default mode pii = %v, want blocked", err)
		}
		warnMode := writepolicy.Enforce(raw, writepolicy.Options{Require: true, Content: writepolicy.ContentOptions{Options: contentcheck.Options{PII: contentcheck.Warn}}})
		warnings, err := write(t, warnMode, "/p.md", 0, sin, tags)
		if err != nil || len(warnings) != 1 || !strings.HasPrefix(warnings[0], "content check: pii/canadian-sin at line 3; fix: remove it") {
			t.Errorf("warn mode pii = %q, %v", warnings, err)
		}
		off := writepolicy.Enforce(raw, writepolicy.Options{Require: true, Content: writepolicy.ContentOptions{Options: contentcheck.Options{PII: contentcheck.Off}}})
		if warnings, err := write(t, off, "/q.md", 0, sin, tags); err != nil || len(warnings) != 0 {
			t.Errorf("off mode pii = %q, %v", warnings, err)
		}
	})
	t.Run("override lands personal data and is reported, never a secret", func(t *testing.T) {
		gated := writepolicy.Enforce(raw, writepolicy.Options{Require: true})
		meta := map[string]string{"tags": "topic", contentcheck.OverrideKey: "test fixture number"}
		warnings, err := write(t, gated, "/sin.md", 0, sin, meta)
		if err != nil {
			t.Fatalf("override: %v", err)
		}
		if doc, err := (backendtest.Direct{Store: raw}).Get("/sin.md", 0); err != nil || doc.Metadata[contentcheck.OverrideKey] != "test fixture number" {
			t.Errorf("override not recorded: %v, %v", err, doc)
		}
		if len(warnings) != 1 || warnings[0] != "content check overridden (test fixture number): pii/canadian-sin at line 3" {
			t.Errorf("warnings = %q", warnings)
		}
		if _, err := write(t, gated, "/sin.md", 1, sin, map[string]string{"tags": "topic", contentcheck.OverrideKey: "  "}); !errors.Is(err, writepolicy.ErrContentBlocked) {
			t.Errorf("blank override = %v, want blocked", err)
		}
		_, err = write(t, gated, "/keys.md", 0, secret, map[string]string{"tags": "topic", contentcheck.OverrideKey: "documented example key"})
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

// write sends one publish with a warning sink and returns what it collected.
func write(t *testing.T, store backend.Store, path string, version int, body []byte, meta map[string]string) ([]string, error) {
	t.Helper()
	var warnings []string
	_, err := store.Publish(context.Background(), backend.WriteRequest{Path: path, ExpectedVersion: version, Content: body, Metadata: meta, Warnings: &warnings})
	return warnings, err
}

func TestEnforceSecretsWarn(t *testing.T) {
	raw := newStore(t)
	if _, err := (backendtest.Direct{Store: raw}).WriteVersion(publishpolicy.DocumentPath, 0, []byte(warnPolicy), policyMeta); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	// The doctor's three findings on the demarkus soul: one container DSN.
	dsn := []byte(storetest.WireSecret)
	placeholder := []byte(storetest.WireSecretFixed)
	sin := []byte("# Person\n\nSIN 046 454 286\n")
	tags := map[string]string{"tags": "postgres"}
	warned := writepolicy.Enforce(raw, writepolicy.Options{Require: true, Content: writepolicy.ContentOptions{
		SecretsWarn: true, Options: contentcheck.Options{PII: contentcheck.Warn},
	}})

	t.Run("a secret lands with the stored remedy, the value never echoed", func(t *testing.T) {
		warnings, err := write(t, warned, "/dsn.md", 0, dsn, tags)
		if err != nil {
			t.Fatalf("warn mode secret: %v", err)
		}
		if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "content check: secret/connection-string-password at line 4; fix: treat the secret as compromised") || strings.Contains(warnings[0], "55432") {
			t.Errorf("warnings = %q", warnings)
		}
	})
	t.Run("the placeholder form is clean", func(t *testing.T) {
		if warnings, err := write(t, warned, "/dsn.md", 1, placeholder, tags); err != nil || len(warnings) != 0 {
			t.Errorf("placeholder = %q, %v; want no warning", warnings, err)
		}
	})
	t.Run("personal data still follows its own mode", func(t *testing.T) {
		blockPII := writepolicy.Enforce(raw, writepolicy.Options{Require: true, Content: writepolicy.ContentOptions{SecretsWarn: true}})
		if _, err := write(t, blockPII, "/p.md", 0, sin, tags); !errors.Is(err, writepolicy.ErrContentBlocked) {
			t.Errorf("pii under secrets warn = %v, want blocked", err)
		}
		warnings, err := write(t, warned, "/p.md", 0, sin, tags)
		if err != nil || len(warnings) != 1 || !strings.HasPrefix(warnings[0], "content check: pii/canadian-sin at line 3; fix: remove it") {
			t.Errorf("pii warn = %q, %v", warnings, err)
		}
	})
}

func TestEnforceReportsWarnLevelPolicyMisses(t *testing.T) {
	raw := newStore(t)
	if _, err := (backendtest.Direct{Store: raw}).WriteVersion(publishpolicy.DocumentPath, 0, []byte("# Write Policy\n\nstrictness: warn\nrequire_tags: category\n"), policyMeta); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	gated := writepolicy.Enforce(raw, writepolicy.Options{Require: true})
	t.Run("a miss under warn lands and is named", func(t *testing.T) {
		warnings, err := write(t, gated, "/a.md", 0, []byte("# A\n"), nil)
		if err != nil {
			t.Fatalf("warn-level miss: %v", err)
		}
		want := "publish policy (warn): missing-tags"
		if len(warnings) != 1 || warnings[0] != want {
			t.Errorf("warnings = %q, want [%q]", warnings, want)
		}
	})
	t.Run("a compliant write leaves the sink empty", func(t *testing.T) {
		if warnings, err := write(t, gated, "/a.md", 1, []byte("# A\n"), map[string]string{"tags": "category:guide"}); err != nil || len(warnings) != 0 {
			t.Errorf("compliant = %q, %v", warnings, err)
		}
	})
	t.Run("no sink is fine", func(t *testing.T) {
		if _, err := (backendtest.Direct{Store: gated}).WriteVersion("/b.md", 0, []byte("# B\n"), nil); err != nil {
			t.Errorf("without a sink: %v", err)
		}
	})
	t.Run("a refusal leaves the sink empty", func(t *testing.T) {
		if _, err := (backendtest.Direct{Store: raw}).WriteVersion(publishpolicy.DocumentPath, 1, []byte("# Write Policy\n\nstrictness: block\nrequire_tags: category\n"), policyMeta); err != nil {
			t.Fatalf("tighten policy: %v", err)
		}
		warnings, err := write(t, gated, "/c.md", 0, []byte("# C\n"), nil)
		if !errors.Is(err, writepolicy.ErrPolicyBlocked) || len(warnings) != 0 {
			t.Errorf("blocked = %q, %v", warnings, err)
		}
	})
}

func TestEnforceFloorExemptPaths(t *testing.T) {
	raw := newStore(t)
	if _, err := (backendtest.Direct{Store: raw}).WriteVersion(publishpolicy.DocumentPath, 0, []byte(warnPolicy), policyMeta); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	floor := publishpolicy.Policy{Strictness: publishpolicy.Block, RequiredFields: []string{"user"}, Exempt: []string{"/graph/**"}}
	gated := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true, Floor: fixedFloor(floor)})}
	// Generated output written under a grant carries no user.
	generated := map[string]string{"tags": "category:federation"}
	if _, err := gated.WriteVersion("/graph/manifest.md", 0, []byte("# Graph\n"), generated); err != nil {
		t.Errorf("exempt graph write: %v", err)
	}
	if _, err := gated.WriteVersion("/notes/a.md", 0, []byte("# A\n"), generated); !errors.Is(err, writepolicy.ErrPolicyBlocked) {
		t.Errorf("write outside the exemption = %v, want blocked", err)
	}
	// The world's own exemption never lifts the floor's rule.
	world := "# Write Policy\n\nstrictness: warn\nexempt: /notes/**\n"
	if _, err := gated.WriteVersion(publishpolicy.DocumentPath, 1, []byte(world), map[string]string{"tags": "category:governance", "user": "a@example.com"}); err != nil {
		t.Fatalf("world policy: %v", err)
	}
	if _, err := gated.WriteVersion("/notes/a.md", 0, []byte("# A\n"), generated); !errors.Is(err, writepolicy.ErrPolicyBlocked) {
		t.Errorf("write under a world exemption = %v, want the floor's block", err)
	}
}
