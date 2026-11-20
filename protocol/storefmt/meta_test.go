package storefmt

import (
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func TestIsReservedMetaKeyCoversEveryRefusedKey(t *testing.T) {
	for _, key := range []string{"previous-hash", "version", "modified", "etag", "status", "warning", "auth", "expected-version"} {
		if !IsReservedMetaKey(key) {
			t.Errorf("IsReservedMetaKey(%q) = false; a publish refuses or consumes it", key)
		}
	}
	for _, key := range []string{"tags", "importance", "owner", "user"} {
		if IsReservedMetaKey(key) {
			t.Errorf("IsReservedMetaKey(%q) = true for publisher metadata", key)
		}
	}
}

// The cap is the publisher's: what the server writes neither counts against
// it nor takes a key, and no stamp value outgrows a WATCH block.
func TestValidateMetaBudgetsPublisherKeysOnly(t *testing.T) {
	full := map[string]string{"tags": "x"}
	full["notes"] = strings.Repeat("n", protocol.MaxMetaBytes-MetaSize(full)-len("notes"))
	full[protocol.MetaUser] = strings.Repeat("u", protocol.MaxStampBytes)
	full[protocol.ClassifierKey] = strings.Repeat("c", protocol.MaxStampBytes)
	if err := ValidateMeta(full); err != nil || MetaSize(full) != protocol.MaxMetaBytes {
		t.Fatalf("a full budget beside full server keys: %v (size %d)", err, MetaSize(full))
	}
	full[protocol.MetaAgent] = "a"
	if err := ValidateMeta(full); !errors.Is(err, ErrInvalidMeta) || !strings.Contains(err.Error(), "metadata too large") {
		t.Errorf("agent is the publisher's and counts: %v", err)
	}
	for _, key := range []string{protocol.MetaAgent, protocol.MetaUser, protocol.ClassifierKey} {
		err := ValidateMeta(map[string]string{key: strings.Repeat("s", protocol.MaxStampBytes+1)})
		if !errors.Is(err, ErrInvalidMeta) || !strings.Contains(err.Error(), "value for key") {
			t.Errorf("%s over the stamp bound: %v", key, err)
		}
	}
	keys := map[string]string{protocol.MetaUser: "u", protocol.ClassifierKey: "c"}
	for i := range protocol.MaxMetaKeys {
		keys["k"+strings.Repeat("0", i)] = "v"
	}
	if err := ValidateMeta(keys); err != nil {
		t.Errorf("%d publisher keys beside the server's: %v", protocol.MaxMetaKeys, err)
	}
	keys["one-more"] = "v"
	if err := ValidateMeta(keys); !errors.Is(err, ErrInvalidMeta) {
		t.Errorf("one key over: %v", err)
	}
}
