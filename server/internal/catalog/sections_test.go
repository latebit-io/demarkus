package catalog

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// hasWord resolves a word through the vocabulary and checks the set.
func hasWord(set []term, word string) bool {
	id, ok := lookupTerm(word)
	return ok && hasTerm(set, id)
}

func collectTokens(field string) []string {
	var out []string
	fieldTokens(field, func(t string) { out = append(out, t) })
	return out
}

func TestFieldTokens(t *testing.T) {
	tests := []struct {
		field string
		want  []string
	}{
		{"path.Match", []string{"path.match", "path", "match"}},
		{"`net.core.rmem_max`,", []string{"net.core.rmem_max", "net", "core", "rmem_max"}},
		{"UDP", []string{"udp"}},
		{"a", nil},
		{"(x)", nil},
		{"read-auth", []string{"read-auth", "read", "auth"}},
		{"---", nil},
		{"Über!", []string{"über"}},
		{"sha256-" + strings.Repeat("a", 64), []string{"sha256"}},
	}
	for _, tt := range tests {
		if got := collectTokens(tt.field); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("fieldTokens(%q) = %v, want %v", tt.field, got, tt.want)
		}
	}
}

func TestQueryTerms(t *testing.T) {
	// An overlong field is queried as its word runs, as it was indexed; the
	// 40-byte run is over the cap on both sides.
	got := queryTerms("  Path.Match udp a UDP `sysctl` auth-" + strings.Repeat("f", 40))
	want := []string{"path.match", "udp", "sysctl", "auth"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("queryTerms = %v, want %v", got, want)
	}
}

func TestStripMarkup(t *testing.T) {
	raw := "> quoted **bold** `code`\n>\n\n- [x] item [link text](/x.md) ![img](a.png)\n\n```sh\n# comment\nrun it\n```\n\n| a | b |\n|---|---|\n| c | d |\n\n<b>tag</b> ~~gone~~ more\n"
	got := stripMarkup(raw)
	// Fence content stays (identifiers and commands are worth matching);
	// only the fence markers go.
	want := "quoted bold code\nitem link text img\n# comment\nrun it\na b\nc d\ntag gone more"
	if got != want {
		t.Errorf("stripMarkup =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "```") {
		t.Errorf("fence markers survived: %q", got)
	}
}

func TestIndexSections(t *testing.T) {
	body := "Preamble text.\n\nSetext Title\n============\n\nunder setext\n\n## Child\n\nchild text\n\n### Grandchild\n\ndeep text\n\n## Sibling\n"
	doc := IndexSections([]byte(body))
	if doc.Len() != 5 {
		t.Fatalf("sections = %d, want 5 (preamble, title, child, grandchild, sibling)", doc.Len())
	}
	s := doc.sections
	if s[0].anchor != "" || s[0].text != "Preamble text." {
		t.Errorf("preamble = %+v", s[0])
	}
	if s[1].anchor != "setext-title" || s[1].text != "under setext" {
		t.Errorf("setext section = anchor %q text %q, want underline dropped", s[1].anchor, s[1].text)
	}
	if !hasWord(s[3].trail, "child") || !hasWord(s[3].trail, "setext") || !hasWord(s[3].trail, "grandchild") {
		t.Errorf("grandchild trail lacks ancestors: %v", s[3].trail)
	}
	if hasWord(s[3].tokens, "child") || !hasWord(s[3].tokens, "deep") {
		t.Errorf("grandchild tokens should be its own text only")
	}
	if s[4].text != "" || s[4].heading != "Sibling" {
		t.Errorf("empty section should still exist for its heading: %+v", s[4])
	}
	if IndexSections([]byte("  \n")).Len() != 0 || IndexSections(nil).Len() != 0 {
		t.Error("blank body should index no sections")
	}
	if got := IndexSections([]byte("no headings, just words")).sections; len(got) != 1 || got[0].anchor != "" {
		t.Errorf("headingless body = %+v, want one bare section", got)
	}
}

func TestSnippet(t *testing.T) {
	text := "first line without it\nsecond line has sysctl and udp\nthird has udp"
	if got := snippet(text, []string{"udp", "sysctl"}); got != "second line has sysctl and udp" {
		t.Errorf("snippet = %q, want the line with most terms", got)
	}
	if got := snippet(text, []string{"nothing"}); got != "first line without it" {
		t.Errorf("snippet with no hit = %q, want the first line", got)
	}
	long := strings.Repeat("ü", 300)
	got := snippet(long, nil)
	if len(got) > SnippetBytes || !strings.HasSuffix(got, "...") || !strings.HasPrefix(got, "üü") || strings.ContainsRune(got, '�') {
		t.Errorf("long snippet = %d bytes %q, want capped on a rune boundary", len(got), got[:12])
	}
}

func TestBodyLookupDocTermsAndArchive(t *testing.T) {
	c := New()
	c.Put("/a.md", map[string]string{"tags": "read-auth", "title": "Private Networks"}, []byte("# Private Networks\n\ntoken gate\n"), time.Now())
	// A term satisfied only by tags or title still needs the other terms in the section.
	rs := mustLookup(t, c, "auth gate", Options{Match: MatchBody})
	if len(rs) != 1 || rs[0].Anchor != "private-networks" || rs[0].Snippet != "token gate" {
		t.Errorf("rows = %+v, want the one section with a snippet", rs)
	}
	if rs = mustLookup(t, c, "auth missing", Options{Match: MatchBody}); len(rs) != 0 {
		t.Errorf("rows = %+v, want none", rs)
	}
	// Set replaces metadata but keeps the body index; Remove drops both.
	c.Set(&Entry{Path: "/a.md", Tags: []string{"other"}, Title: "Renamed"})
	if rs = mustLookup(t, c, "gate", Options{Match: MatchBody}); len(rs) != 1 || rs[0].Title != "Renamed" {
		t.Errorf("after Set: rows = %+v", rs)
	}
	if rs = mustLookup(t, c, "auth gate", Options{Match: MatchBody}); len(rs) != 0 {
		t.Errorf("old tag still matches after Set: %+v", rs)
	}
	c.Remove("/a.md")
	if c.Sections("/a.md") != nil || len(mustLookup(t, c, "gate", Options{Match: MatchBody})) != 0 {
		t.Error("Remove left the section index behind")
	}
}
