package storefmt

import "testing"

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
