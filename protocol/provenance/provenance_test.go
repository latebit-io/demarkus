package provenance

import (
	"slices"
	"testing"
)

func TestParsePin(t *testing.T) {
	const sha = "3f2c9e1a"
	tests := []struct {
		ref  string
		want Pin
	}{
		{"/adr/0006.md/v5", Pin{kind: document, Source: "/adr/0006.md", Version: "5"}},
		{"../spec.md/v12#rules", Pin{kind: document, Source: "../spec.md", Version: "12"}},
		{"mark://latebit/projects/a.md/v3", Pin{kind: document, Source: "mark://latebit/projects/a.md", Version: "3"}},
		{"/a.md/v007", Pin{kind: document, Source: "/a.md", Version: "7"}},
		{"/a.md", Pin{kind: document, Source: "/a.md"}},
		{"/a.md/v0", Pin{kind: document, Source: "/a.md/v0"}},
		{"/a.md/v+2", Pin{kind: document, Source: "/a.md/v+2"}},
		{"/api/v2", Pin{kind: document, Source: "/api/v2"}},
		{"https://github.com/latebit-io/demarkus-library/tree/" + sha + "/internal/reader", Pin{kind: commit, Source: "https://github.com/latebit-io/demarkus-library/internal/reader", Version: sha}},
		{"https://github.com/latebit-io/demarkus-library/blob/" + sha + "/go.mod", Pin{kind: commit, Source: "https://github.com/latebit-io/demarkus-library/go.mod", Version: sha}},
		{"https://github.com/latebit-io/demarkus-library/commit/" + sha, Pin{kind: commit, Source: "https://github.com/latebit-io/demarkus-library", Version: sha}},
		{"https://github.com/latebit-io/demarkus-library.git", Pin{kind: commit, Source: "https://github.com/latebit-io/demarkus-library"}},
		{"https://github.com/latebit-io/demarkus-library/tree/main/internal", Pin{kind: commit, Source: "https://github.com/latebit-io/demarkus-library/internal"}},
		{"https://github.com/latebit-io", Pin{kind: opaque, Source: "https://github.com/latebit-io"}},
		{"https://en.wikipedia.org/w/index.php?oldid=1358907368", Pin{kind: opaque, Source: "https://en.wikipedia.org/w/index.php?oldid=1358907368"}},
		{"/a b.md/v1", Pin{kind: malformed, Source: "/a b.md/v1"}},
	}
	for _, tt := range tests {
		tt.want.Ref = tt.ref
		if got := ParsePin(tt.ref); got != tt.want {
			t.Errorf("ParsePin(%q) = %+v, want %+v", tt.ref, got, tt.want)
		}
	}
}

func TestUnpinnedNamesEveryMovingTarget(t *testing.T) {
	value := "/a.md/v2, /b.md, https://github.com/o/r/tree/main, https://example.com/x, /c d.md,"
	want := []string{"/b.md", "https://github.com/o/r/tree/main", "/c d.md"}
	if got := Unpinned(value); !slices.Equal(got, want) {
		t.Errorf("Unpinned = %q, want %q", got, want)
	}
	if got := Unpinned(""); got != nil {
		t.Errorf("Unpinned(\"\") = %q, want nil", got)
	}
}

func TestLocalResolvesPathPinsOnly(t *testing.T) {
	tests := []struct {
		ref, doc, want string
	}{
		{"/adr/x.md/v1", "/notes/a.md", "/adr/x.md"},
		{"../adr/x.md/v1", "/notes/a.md", "/adr/x.md"},
		{"x.md/v1", "/notes/a.md", "/notes/x.md"},
		{"mark://w/x.md/v1", "/notes/a.md", ""},
		{"https://github.com/o/r/commit/abcdef0", "/a.md", ""},
	}
	for _, tt := range tests {
		if got := ParsePin(tt.ref).Local(tt.doc); got != tt.want {
			t.Errorf("Local(%q, %q) = %q, want %q", tt.ref, tt.doc, got, tt.want)
		}
	}
}

func TestUnder(t *testing.T) {
	tests := []struct {
		ref, doc, source string
		want             bool
	}{
		{"/adr/x.md/v1", "/a.md", "/adr/x.md", true},
		{"x.md/v1", "/adr/a.md", "/adr/x.md", true},
		{"/adr/x.md/v1", "/a.md", "/adr/", true},
		{"/adr/x.md/v1", "/a.md", "/adr", true},
		{"/adr/x.md/v1", "/a.md", "/ad", false},
		{"/adr/xy.md/v1", "/a.md", "/adr/x.md", false},
		{"https://github.com/o/r/tree/abcdef0/src", "/a.md", "https://github.com/o/r", true},
		{"https://github.com/o/r/blob/abcdef0/src/a.go", "/a.md", "https://github.com/o/r/src", true},
		{"https://github.com/o/r/blob/abcdef0/src/a.go", "/a.md", "https://github.com/o/r/lib", false},
		{"https://github.com/o/r2/tree/abcdef0", "/a.md", "https://github.com/o/r", false},
		{"mark://w/x.md/v2", "/a.md", "mark://w/x.md", true},
		{"/adr/x.md/v1", "/a.md", "/", true},
		{"/adr", "/a.md", "/adr/", false},
	}
	for _, tt := range tests {
		if got := Under(ParsePin(tt.ref).Target(tt.doc), tt.source); got != tt.want {
			t.Errorf("Under(%q in %q, %q) = %v, want %v", tt.ref, tt.doc, tt.source, got, tt.want)
		}
	}
}

func TestDocumentVersion(t *testing.T) {
	if got := ParsePin("/a.md/v9").DocumentVersion(); got != 9 {
		t.Errorf("DocumentVersion = %d, want 9", got)
	}
	if got := ParsePin("/a.md").DocumentVersion(); got != 0 {
		t.Errorf("DocumentVersion(unpinned) = %d, want 0", got)
	}
	if got := ParsePin("https://github.com/o/r/commit/abcdef0").DocumentVersion(); got != 0 {
		t.Errorf("DocumentVersion(commit) = %d, want 0", got)
	}
}

func TestDrift(t *testing.T) {
	tests := []struct {
		pinned, current int
		problem, fix    string
	}{
		{3, 5, "is at v5, past the pinned v3", "read what changed, update, pin v5"},
		{9, 5, "has no v9; it is at v5", "pin the version it was written from"},
		{5, 5, "", ""},
	}
	for _, tt := range tests {
		if problem, fix := Drift(tt.pinned, tt.current); problem != tt.problem || fix != tt.fix {
			t.Errorf("Drift(%d, %d) = %q, %q; want %q, %q", tt.pinned, tt.current, problem, fix, tt.problem, tt.fix)
		}
	}
}
