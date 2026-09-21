package storefmt

import (
	"bytes"
	"testing"
)

// Whatever SerializeVersion writes, InspectStoredVersion must accept and agree with.
func FuzzStoredVersionRoundTrip(f *testing.F) {
	f.Add(1, []byte(nil), []byte("# Doc\n"), "title", "Doc")
	f.Add(2, []byte("---\nversion: 1\n---\nold"), []byte("---\nfence in body\n---\n"), "", "")
	f.Fuzz(func(t *testing.T, version int, prev, content []byte, key, value string) {
		meta := map[string]string{}
		if key != "" {
			meta[key] = value
		}
		stored, err := SerializeVersion(version, prev, content, meta)
		if err != nil {
			t.Skip()
		}
		header, err := InspectStoredVersion(stored)
		if err != nil {
			t.Fatalf("writer output rejected: %v\n%q", err, stored)
		}
		if header.Version != version {
			t.Fatalf("version: got %d, want %d", header.Version, version)
		}
		if !bytes.HasSuffix(stored, content) {
			t.Fatalf("content not preserved verbatim: %q", stored)
		}
	})
}

func FuzzInspectStoredVersion(f *testing.F) {
	f.Add([]byte("---\nversion: 1\narchived: false\n---\nbody"))
	f.Add([]byte("---\n---\n"))
	f.Fuzz(func(t *testing.T, stored []byte) {
		header, err := InspectStoredVersion(stored)
		if err == nil && header.Version < 1 {
			t.Fatalf("accepted version %d", header.Version)
		}
	})
}
