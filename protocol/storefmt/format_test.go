package storefmt

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

// invalidDocPaths name no document, so a version neither writes nor accepts one.
var invalidDocPaths = []string{
	"", "/", "a.md", "/a//b.md", "/a/./b.md", "/a/../b.md", "/a/", "/a\nb.md", "/a\rb.md", "/a\\b.md", "/a\x00b.md",
	"/" + strings.Repeat("a", protocol.MaxRequestPathLength),
}

func TestStoredVersionNumber(t *testing.T) {
	type versionCase struct {
		name    string
		stored  string
		want    int
		wantErr bool
	}
	tests := []versionCase{
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
		{name: "v1 path", stored: "---\nversion: 1\narchived: false\npath: /a.md\n---\n", want: 1},
		{name: "v2 path", stored: "---\nversion: 2\narchived: false\nprevious-hash: sha256-" + strings.Repeat("a", 64) + "\npath: /a.md\n---\n", want: 2},
		{name: "path before archived", stored: "---\nversion: 1\npath: /a.md\narchived: false\n---\n", wantErr: true},
		{name: "path before previous hash", stored: "---\nversion: 2\narchived: false\npath: /a.md\nprevious-hash: sha256-" + strings.Repeat("a", 64) + "\n---\n", wantErr: true},
		{name: "path after metadata", stored: "---\nversion: 1\narchived: false\nmeta.a: value\npath: /a.md\n---\n", wantErr: true},
		{name: "duplicate path", stored: "---\nversion: 1\narchived: false\npath: /a.md\npath: /a.md\n---\n", wantErr: true},
		{name: "path carriage return", stored: "---\nversion: 1\narchived: false\npath: /a.md\r\n---\n", wantErr: true},
	}
	pathCases := make([]versionCase, 0, len(invalidDocPaths))
	for _, docPath := range invalidDocPaths {
		pathCases = append(pathCases, versionCase{name: fmt.Sprintf("path %q", docPath), stored: "---\nversion: 1\narchived: false\npath: " + docPath + "\n---\n", wantErr: true})
	}
	for _, test := range slices.Concat(tests, pathCases) {
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
		if _, err := SerializeVersion(&VersionSpec{Path: "/doc.md", Version: version}); err == nil {
			t.Errorf("SerializeVersion(%d) succeeded", version)
		}
	}
	if _, err := SerializeVersion(&VersionSpec{Path: "/doc.md", Version: MaxVersionNumber, Previous: []byte("previous")}); err != nil {
		t.Errorf("SerializeVersion(max): %v", err)
	}
	if strconv.IntSize > 32 {
		if _, err := SerializeVersion(&VersionSpec{Path: "/doc.md", Version: MaxVersionNumber + 1, Previous: []byte("previous")}); err == nil {
			t.Error("SerializeVersion(max+1) succeeded")
		}
	}
}

func TestSerializeVersionRejectsInvalidUTF8Metadata(t *testing.T) {
	invalid := string([]byte{'v', 0xff})
	if _, err := SerializeVersion(&VersionSpec{Path: "/doc.md", Version: 1, Content: []byte("body"), Metadata: map[string]string{"title": invalid}}); !errors.Is(err, ErrInvalidMeta) {
		t.Fatalf("SerializeVersion error = %v, want ErrInvalidMeta", err)
	}
}

// A version names its document in a store field after the chain link; a
// legacy version names none, and a publisher "path" is stored as meta.path.
func TestStoredPath(t *testing.T) {
	v1 := "---\nversion: 1\narchived: false\npath: /docs/a.md\ntitle: A\n---\nbody"
	tests := []struct {
		name     string
		spec     *VersionSpec // nil for a stored legacy version
		stored   string
		version  int
		wantPath string
		wantMeta map[string]string
	}{
		{
			name: "v1", spec: &VersionSpec{Path: "/docs/a.md", Version: 1, Content: []byte("body"), Metadata: map[string]string{"title": "A"}},
			stored: v1, version: 1, wantPath: "/docs/a.md", wantMeta: map[string]string{"title": "A"},
		},
		{
			name: "v2", spec: &VersionSpec{Path: "/docs/a.md", Version: 2, Previous: []byte(v1), Content: []byte("body 2")},
			stored:  "---\nversion: 2\narchived: false\nprevious-hash: sha256-" + StoredETag([]byte(v1)) + "\npath: /docs/a.md\n---\nbody 2",
			version: 2, wantPath: "/docs/a.md",
		},
		{
			name: "publisher path", spec: &VersionSpec{Path: "/store.md", Version: 1, Content: []byte("body"), Metadata: map[string]string{"path": "x"}},
			stored: "---\nversion: 1\narchived: false\npath: /store.md\nmeta.path: x\n---\nbody", version: 1, wantPath: "/store.md", wantMeta: map[string]string{"path": "x"},
		},
		{
			name:    "legacy",
			stored:  "---\nversion: 2\narchived: false\nprevious-hash: sha256-" + strings.Repeat("c", 64) + "\ntitle: Old\n---\nbody",
			version: 2, wantMeta: map[string]string{"title": "Old"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stored := []byte(test.stored)
			if test.spec != nil {
				var err error
				if stored, err = SerializeVersion(test.spec); err != nil || string(stored) != test.stored {
					t.Fatalf("SerializeVersion = (%q, %v), want %q", stored, err, test.stored)
				}
			}
			header, err := InspectStoredVersion(stored)
			if err != nil || header.Version != test.version || header.Path != test.wantPath {
				t.Errorf("InspectStoredVersion = (%+v, %v), want v%d path %q", header, err, test.version, test.wantPath)
			}
			if meta := ExtractMetadata(stored); !maps.Equal(meta, test.wantMeta) {
				t.Errorf("metadata = %v, want %v", meta, test.wantMeta)
			}
		})
	}
}

func TestCheckNames(t *testing.T) {
	named, legacy := StoredVersionHeader{Version: 2, Path: "/a.md"}, StoredVersionHeader{Version: 2}
	tests := []struct {
		name    string
		header  StoredVersionHeader
		docPath string
		version int
		wantErr bool
	}{
		{name: "match", header: named, docPath: "/a.md", version: 2},
		{name: "any version", header: named, docPath: "/a.md"},
		{name: "other path", header: named, docPath: "/b.md", version: 2, wantErr: true},
		{name: "other version", header: named, docPath: "/a.md", version: 3, wantErr: true},
		{name: "legacy", header: legacy, docPath: "/b.md", version: 2},
		{name: "legacy other version", header: legacy, docPath: "/b.md", version: 3, wantErr: true},
	}
	for _, test := range tests {
		if err := test.header.CheckNames(test.docPath, test.version); (err != nil) != test.wantErr {
			t.Errorf("%s: CheckNames = %v, want error %t", test.name, err, test.wantErr)
		}
	}
}

func TestSerializeVersionRejectsInvalidPath(t *testing.T) {
	for _, docPath := range invalidDocPaths {
		if stored, err := SerializeVersion(&VersionSpec{Path: docPath, Version: 1, Content: []byte("body")}); err == nil {
			t.Errorf("SerializeVersion(%q) = %q, want error", docPath, stored)
		}
	}
}

// The longest path beside near-full metadata still fits MaxStoreFrontmatter,
// which readers size their header reads by.
func TestMaxStoreFrontmatterHoldsLongestPath(t *testing.T) {
	meta := make(map[string]string, protocol.MaxMetaKeys)
	for i := range protocol.MaxMetaKeys {
		meta[fmt.Sprintf("k%02d", i)] = strings.Repeat("v", protocol.MaxMetaBytes/protocol.MaxMetaKeys-3)
	}
	longest := "/" + strings.Repeat("a", protocol.MaxRequestPathLength-1)
	stored, err := SerializeVersion(&VersionSpec{Path: longest, Version: MaxVersionNumber, Previous: []byte("previous"), Metadata: meta})
	if err != nil {
		t.Fatalf("SerializeVersion(longest path): %v", err)
	}
	if block := len(stored) - len(protocol.FrontmatterFence)*2; block > MaxStoreFrontmatter {
		t.Errorf("frontmatter %d bytes exceeds MaxStoreFrontmatter %d", block, MaxStoreFrontmatter)
	}
}
