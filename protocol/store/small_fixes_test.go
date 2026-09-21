package store

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// A directory whose name merely starts with two dots is inside the root.
func TestPruneUnderDotDotPrefixedDirectory(t *testing.T) {
	s := New(t.TempDir())
	meta := map[string]string{"retention": "1"}
	for i := range 3 {
		doc, err := s.Write("/..notes/a.md", []byte(strings.Repeat("x", i+1)), meta)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if doc.Prune != nil && doc.Prune.Err != nil {
			t.Fatalf("write %d: prune error: %v", i, doc.Prune.Err)
		}
	}
	versions, err := s.Versions("/..notes/a.md")
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(versions) != 1 {
		t.Errorf("versions kept = %d, want 1", len(versions))
	}
}

func TestPartialWalkErrorZeroValue(t *testing.T) {
	var e storefmt.PartialWalkError
	if got := e.Error(); !strings.Contains(got, "0 entries") {
		t.Errorf("Error() = %q", got)
	}
}

// ApplyOKFTypeDefault never hands back the caller's map, whichever branch runs.
func TestApplyOKFTypeDefaultAlwaysCopies(t *testing.T) {
	tests := []struct {
		name    string
		reqPath string
		meta    map[string]string
	}{
		{name: "reserved file", reqPath: "/index.md", meta: map[string]string{"tags": "a"}},
		{name: "declared type", reqPath: "/doc.md", meta: map[string]string{"type": "Decision"}},
		{name: "defaulted", reqPath: "/doc.md", meta: map[string]string{"tags": "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := storefmt.ApplyOKFTypeDefault(tt.reqPath, tt.meta)
			got["probe"] = "x"
			if _, leaked := tt.meta["probe"]; leaked {
				t.Error("result aliases the caller's map")
			}
		})
	}
	if got := storefmt.ApplyOKFTypeDefault("/index.md", nil); got != nil {
		t.Errorf("nil metadata on a reserved file = %v, want nil", got)
	}
}

// A write check sees the persisted form, and its refusal leaves no version and
// no per document directory behind.
func TestWriteCheckedRefusalLeavesNothing(t *testing.T) {
	s := New(t.TempDir())
	errRefused := errors.New("refused")
	var seen storefmt.PreparedWrite
	spec := &storefmt.WriteSpec{
		Path: "/notes/doc.md", Content: []byte("# Doc\n"), Metadata: map[string]string{"tags": "b, a"},
		Check: func(write storefmt.PreparedWrite) error { seen = write; return errRefused },
	}
	if _, err := s.WriteChecked(spec); !errors.Is(err, errRefused) {
		t.Fatalf("WriteChecked: %v, want the check's error", err)
	}
	if seen.Path != "/notes/doc.md" || seen.Metadata["tags"] != "b,a" || string(seen.Content) != "# Doc\n" {
		t.Errorf("check saw %+v", seen)
	}
	if version, err := s.CurrentVersionResult("/notes/doc.md"); err != nil || version != 0 {
		t.Errorf("version after refusal = %d, %v", version, err)
	}
	loc, err := s.locate("/notes/doc.md")
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if _, err := os.Stat(loc.docDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("per document directory after refusal: %v, want absent", err)
	}
	spec.Check = nil
	if doc, err := s.WriteChecked(spec); err != nil || doc.Version != 1 {
		t.Errorf("unchecked write = %+v, %v", doc, err)
	}
}
