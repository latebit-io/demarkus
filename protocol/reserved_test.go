package protocol

import "testing"

func TestIsReservedMetadataKey(t *testing.T) {
	for _, key := range []string{"version", "etag", "status", "matches"} {
		if !IsReservedMetadataKey(key) {
			t.Errorf("%q should be reserved", key)
		}
	}
	for _, key := range []string{"tags", "title", ""} {
		if IsReservedMetadataKey(key) {
			t.Errorf("%q should not be reserved", key)
		}
	}
}
