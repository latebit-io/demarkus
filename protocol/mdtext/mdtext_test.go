package mdtext

import (
	"reflect"
	"strings"
	"testing"
)

func collectTokens(field string) []string {
	var out []string
	Tokens(field, func(t string) { out = append(out, t) })
	return out
}

func TestTokens(t *testing.T) {
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
			t.Errorf("Tokens(%q) = %v, want %v", tt.field, got, tt.want)
		}
	}
}

func TestQueryTerms(t *testing.T) {
	// An overlong field is queried as its word runs, as it was indexed; the
	// 40-byte run is over the cap on both sides.
	got := QueryTerms("  Path.Match udp a UDP `sysctl` auth-" + strings.Repeat("f", 40))
	want := []string{"path.match", "udp", "sysctl", "auth"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("QueryTerms = %v, want %v", got, want)
	}
}

func TestStrip(t *testing.T) {
	raw := "> quoted **bold** `code`\n>\n\n- [x] item [link text](/x.md) ![img](a.png)\n\n```sh\n# comment\nrun it\n```\n\n| a | b |\n|---|---|\n| c | d |\n\n<b>tag</b> ~~gone~~ more\n"
	got := Strip(raw)
	// Fence content stays (identifiers and commands are worth matching);
	// only the fence markers go.
	want := "quoted bold code\nitem link text img\n# comment\nrun it\na b\nc d\ntag gone more"
	if got != want {
		t.Errorf("Strip =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "```") {
		t.Errorf("fence markers survived: %q", got)
	}
}

func TestSplit(t *testing.T) {
	src := "intro line\n\n# Title\n\ntop text\n\n## Alpha\n\nalpha text\n\n### Deep\n\ndeep text\n\n## Beta\n\nbeta text\n"
	got := Split(src)
	type row struct {
		anchor, heading, own string
		trail                []string
	}
	rows := make([]row, 0, len(got))
	for _, p := range got {
		rows = append(rows, row{p.Anchor, p.Heading, strings.TrimSpace(p.Own), p.Trail})
	}
	want := []row{
		{"", "", "intro line", nil},
		{"title", "Title", "top text", []string{"Title"}},
		{"alpha", "Alpha", "alpha text", []string{"Title", "Alpha"}},
		{"deep", "Deep", "deep text", []string{"Title", "Alpha", "Deep"}},
		{"beta", "Beta", "beta text", []string{"Title", "Beta"}},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("Split =\n%+v\nwant\n%+v", rows, want)
	}
	if got := Split("\n\n# Only\n\nbody\n"); len(got) != 1 || got[0].Anchor != "only" {
		t.Errorf("blank preamble kept: %+v", got)
	}
	if got := Split("no headings at all"); len(got) != 1 || got[0].Anchor != "" || got[0].Own != "no headings at all" {
		t.Errorf("headingless body = %+v", got)
	}
}

func TestCounts(t *testing.T) {
	counts, total := Counts(Strip("alpha beta alpha"))
	if total != 3 || counts["alpha"] != 2 || counts["beta"] != 1 {
		t.Errorf("Counts = %v, %d", counts, total)
	}
}
