package gate

import (
	"slices"
	"testing"

	"github.com/latebit-io/demarkus/protocol/mdoutline"
)

// One helper says where a heading's own lines end, so no edit lands between
// a setext title and its underline.
func TestHeadingEndCoversASetextUnderline(t *testing.T) {
	for _, tc := range []struct{ name, body, rest string }{
		{"atx", "# Title\n\nSummary.\n", "\n\nSummary.\n"},
		{"atx at the end", "# Title", ""},
		{"setext h1", "Title\n=====\n\nSummary.\n", "\n\nSummary.\n"},
		{"setext h2 after text", "# T\n\nSection\n-------\nbody\n", "\nbody\n"},
		{"setext over two lines", "A long\ntitle\n===\nbody\n", "\nbody\n"},
		{"setext with CRLF", "Title\r\n=====\r\nbody\r\n", "\nbody\r\n"},
	} {
		headings := mdoutline.Headings(tc.body)
		h := headings[len(headings)-1]
		if got := tc.body[HeadingEnd(tc.body, h):]; got != tc.rest {
			t.Errorf("%s: after the heading = %q, want %q", tc.name, got, tc.rest)
		}
	}
}

// One rule says what a hub is: by name, by the directory beside it, by body.
func TestIsHubByNameDirectoryOrBody(t *testing.T) {
	dirs := func(dir string) bool { return dir == "/a/b/" }
	for _, tc := range []struct {
		path, body string
		hasDir     func(string) bool
		want       bool
	}{
		{"/a/index.md", "prose", nil, true},
		{"/a/b.md", "prose", dirs, true},
		{"/a/b.md", "prose", nil, false},
		{"/a/c.md", "prose", dirs, false},
		{"/a/c.md", "# C\n\n" + linkBullets(6), nil, true},
	} {
		if got := IsHub(tc.path, tc.body, tc.hasDir); got != tc.want {
			t.Errorf("IsHub(%s) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestLiftKeepsWhatTheDocumentCarries(t *testing.T) {
	meta := map[string]string{"tags": "kept"}
	keys := Lift(map[string]string{"Name": "Title", "tags": "dropped", "type": " Guide ", "version": "9", "empty": " "}, meta)
	if !slices.Equal(keys, []string{"title", "type"}) || meta["title"] != "Title" || meta["type"] != "Guide" || meta["tags"] != "kept" {
		t.Errorf("lifted %v into %v", keys, meta)
	}
}
