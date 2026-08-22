package store

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestStoredVersionNumber(t *testing.T) {
	tests := []struct {
		name    string
		stored  string
		want    int
		wantErr bool
	}{
		{name: "canonical", stored: "---\nversion: 12\narchived: false\nprevious-hash: sha256-" + strings.Repeat("a", 64) + "\n---\nbody", want: 12},
		{name: "no frontmatter", stored: "body", wantErr: true},
		{name: "unterminated", stored: "---\nversion: 1\n", wantErr: true},
		{name: "missing", stored: "---\narchived: false\n---\n", wantErr: true},
		{name: "empty", stored: "---\nversion: \narchived: false\n---\n", wantErr: true},
		{name: "zero", stored: "---\nversion: 0\narchived: false\n---\n", wantErr: true},
		{name: "negative", stored: "---\nversion: -1\narchived: false\n---\n", wantErr: true},
		{name: "plus", stored: "---\nversion: +1\narchived: false\n---\n", wantErr: true},
		{name: "leading zero", stored: "---\nversion: 01\narchived: false\n---\n", wantErr: true},
		{name: "trailing space", stored: "---\nversion: 1 \narchived: false\n---\n", wantErr: true},
		{name: "float", stored: "---\nversion: 1.0\narchived: false\n---\n", wantErr: true},
		{name: "overflow", stored: "---\nversion: 999999999999999999999999\narchived: false\n---\n", wantErr: true},
		{name: "duplicate", stored: "---\nversion: 1\nversion: 1\narchived: false\n---\n", wantErr: true},
		{name: "malformed duplicate", stored: "---\nversion: 1\nversion:2\narchived: false\n---\n", wantErr: true},
		{name: "missing separator", stored: "---\nversion:1\narchived: false\n---\n", wantErr: true},
		{name: "spaced key", stored: "---\nversion : 1\narchived: false\n---\n", wantErr: true},
		{name: "missing archived", stored: "---\nversion: 1\n---\n", wantErr: true},
		{name: "duplicate archived", stored: "---\nversion: 1\narchived: false\narchived: false\n---\n", wantErr: true},
		{name: "v1 previous hash", stored: "---\nversion: 1\narchived: false\nprevious-hash: sha256-" + strings.Repeat("a", 64) + "\n---\n", wantErr: true},
		{name: "v2 missing previous hash", stored: "---\nversion: 2\narchived: false\n---\n", wantErr: true},
		{name: "bad previous hash", stored: "---\nversion: 2\narchived: false\nprevious-hash: sha256-nope\n---\n", wantErr: true},
		{name: "unknown bare field", stored: "---\nversion: 1\narchived: false\nevil: value\n---\n", wantErr: true},
		{name: "unsorted metadata", stored: "---\nversion: 1\narchived: false\nmeta.z: value\nmeta.a: value\n---\n", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := StoredVersionNumber([]byte(test.stored))
			if (err != nil) != test.wantErr || got != test.want {
				t.Errorf("StoredVersionNumber() = (%d, %v), want (%d, error=%v)", got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestInspectStoredVersionHeader(t *testing.T) {
	previousHash := "sha256-" + strings.Repeat("b", 64)
	stored := []byte("---\nversion: 2\narchived: true\nprevious-hash: " + previousHash + "\n---\nbody")

	header, err := InspectStoredVersion(stored)
	if err != nil {
		t.Fatalf("InspectStoredVersion: %v", err)
	}
	if header.Version != 2 || !header.Archived || header.PreviousHash != previousHash {
		t.Errorf("header = %+v, want version 2, archived, previous-hash %q", header, previousHash)
	}
}

func TestInspectStoredVersionRejectsPrefixedReservedAliases(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "reserved version", key: "version"},
		{name: "reserved previous hash", key: "previous-hash"},
		{name: "reserved archived", key: "archived"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stored := []byte("---\nversion: 1\narchived: false\nmeta." + test.key + ": value\n---\nbody")
			if _, err := InspectStoredVersion(stored); err == nil {
				t.Fatalf("InspectStoredVersion accepted meta.%s", test.key)
			}
		})
	}
}

func TestInspectStoredVersionAcceptsLegacyPrefixedOKFFields(t *testing.T) {
	stored := []byte("---\nversion: 1\narchived: false\nmeta.tags: alpha,beta\nmeta.type: knowledge\n---\nbody")
	if _, err := InspectStoredVersion(stored); err != nil {
		t.Fatalf("InspectStoredVersion rejected legacy OKF metadata: %v", err)
	}
}

func TestInspectStoredVersionRejectsMixedOKFAliases(t *testing.T) {
	stored := []byte("---\nversion: 1\narchived: false\ntags: [alpha, beta]\nmeta.tags: alpha,beta\n---\nbody")
	if _, err := InspectStoredVersion(stored); err == nil {
		t.Fatal("InspectStoredVersion accepted canonical and legacy tags together")
	}
}

func TestInspectStoredVersionAcceptsCustomMetadata(t *testing.T) {
	stored := []byte("---\nversion: 1\narchived: false\nmeta.custom: value\n---\nbody")
	if _, err := InspectStoredVersion(stored); err != nil {
		t.Fatalf("InspectStoredVersion: %v", err)
	}
}

func TestNormalizeMetadata(t *testing.T) {
	meta := map[string]string{"tags": " alpha, beta ", "title": "Kept"}
	normalized := NormalizeMetadata(meta)
	if normalized["tags"] != "alpha,beta" || normalized["title"] != "Kept" {
		t.Errorf("normalized metadata = %v", normalized)
	}
	if meta["tags"] != " alpha, beta " {
		t.Errorf("input metadata mutated: %v", meta)
	}
}

func TestSerializeVersionRange(t *testing.T) {
	for _, version := range []int{-1, 0} {
		if _, err := SerializeVersion(version, nil, nil, nil); err == nil {
			t.Errorf("SerializeVersion(%d) succeeded", version)
		}
	}
	if _, err := SerializeVersion(MaxVersionNumber, []byte("previous"), nil, nil); err != nil {
		t.Errorf("SerializeVersion(max): %v", err)
	}
	if strconv.IntSize > 32 {
		if _, err := SerializeVersion(MaxVersionNumber+1, []byte("previous"), nil, nil); err == nil {
			t.Error("SerializeVersion(max+1) succeeded")
		}
	}
}

func TestSerializeVersionRejectsInvalidUTF8Metadata(t *testing.T) {
	invalid := string([]byte{'v', 0xff})
	if _, err := SerializeVersion(1, nil, []byte("body"), map[string]string{"title": invalid}); !errors.Is(err, ErrInvalidMeta) {
		t.Fatalf("SerializeVersion error = %v, want ErrInvalidMeta", err)
	}
}
