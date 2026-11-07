package links

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestExtract(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "no links",
			body: "Just some text.",
			want: nil,
		},
		{
			name: "single link",
			body: "See [other](other.md) for details.",
			want: []string{"other.md"},
		},
		{
			name: "multiple links",
			body: "Go to [a](a.md) and [b](/b.md) and [c](mark://host/c.md).",
			want: []string{"a.md", "/b.md", "mark://host/c.md"},
		},
		{
			name: "fragment only links are excluded",
			body: "See [section](#overview) above.",
			want: nil,
		},
		{
			name: "mixed fragment and real links",
			body: "See [overview](#top) and [guide](guide.md).",
			want: []string{"guide.md"},
		},
		{
			name: "fragment stripped from destination",
			body: "See [intro](doc.md#intro) and [doc](doc.md).",
			want: []string{"doc.md", "doc.md"},
		},
		{
			name: "empty body",
			body: "",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Extract(tt.body)
			if len(got) != len(tt.want) {
				t.Fatalf("Extract() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Extract()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestExtractWithPositions(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []LinkInfo
	}{
		{
			name: "no links",
			body: "Just some text.",
			want: nil,
		},
		{
			name: "single link",
			body: "See [other](other.md) for details.",
			want: []LinkInfo{
				{Dest: "other.md", Text: "other", OpenBracket: 4, CloseBracket: 10},
			},
		},
		{
			name: "multiple links",
			body: "[a](a.md) and [b](/b.md)",
			want: []LinkInfo{
				{Dest: "a.md", Text: "a", OpenBracket: 0, CloseBracket: 2},
				{Dest: "/b.md", Text: "b", OpenBracket: 14, CloseBracket: 16},
			},
		},
		{
			name: "fragment only excluded",
			body: "See [section](#overview).",
			want: nil,
		},
		{
			name: "fragment stripped from destination",
			body: "[intro](doc.md#intro)",
			want: []LinkInfo{
				{Dest: "doc.md", Text: "intro", OpenBracket: 0, CloseBracket: 6},
			},
		},
		{
			name: "link with bold text",
			body: "[hello **world**](url.md)",
			want: []LinkInfo{
				{Dest: "url.md", Text: "hello world", OpenBracket: 0, CloseBracket: 16},
			},
		},
		{
			// The link's parent is an inline (Emphasis); the blockStart walk
			// must skip inline ancestors, whose Lines() panics by contract.
			name: "link inside bold",
			body: "**[hello](url.md)**",
			want: []LinkInfo{
				{Dest: "url.md", Text: "hello", OpenBracket: 2, CloseBracket: 8},
			},
		},
		{
			// Links in a later block carry that block's start; the empty-label
			// link keeps it even though its bracket positions are unknown.
			name: "block start after leading paragraph",
			body: "Intro paragraph.\n\n[link](a.md) and [](b.md)\n",
			want: []LinkInfo{
				{Dest: "a.md", Text: "link", OpenBracket: 18, CloseBracket: 23, BlockStart: 18},
				{Dest: "b.md", Text: "", OpenBracket: -1, CloseBracket: -1, BlockStart: 18},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractWithPositions(tt.body)
			if len(got) != len(tt.want) {
				t.Fatalf("ExtractWithPositions() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i].Dest != tt.want[i].Dest {
					t.Errorf("[%d].Dest = %q, want %q", i, got[i].Dest, tt.want[i].Dest)
				}
				if got[i].Text != tt.want[i].Text {
					t.Errorf("[%d].Text = %q, want %q", i, got[i].Text, tt.want[i].Text)
				}
				if got[i].OpenBracket != tt.want[i].OpenBracket {
					t.Errorf("[%d].OpenBracket = %d, want %d", i, got[i].OpenBracket, tt.want[i].OpenBracket)
				}
				if got[i].CloseBracket != tt.want[i].CloseBracket {
					t.Errorf("[%d].CloseBracket = %d, want %d", i, got[i].CloseBracket, tt.want[i].CloseBracket)
				}
				if got[i].BlockStart != tt.want[i].BlockStart {
					t.Errorf("[%d].BlockStart = %d, want %d", i, got[i].BlockStart, tt.want[i].BlockStart)
				}
			}
		})
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		dest    string
		want    string
	}{
		{
			name:    "already absolute",
			baseURL: "mark://host:6309/dir/page.md",
			dest:    "mark://other:6309/doc.md",
			want:    "mark://other:6309/doc.md",
		},
		{
			name:    "relative sibling",
			baseURL: "mark://host:6309/dir/page.md",
			dest:    "other.md",
			want:    "mark://host:6309/dir/other.md",
		},
		{
			name:    "absolute path",
			baseURL: "mark://host:6309/dir/page.md",
			dest:    "/root.md",
			want:    "mark://host:6309/root.md",
		},
		{
			name:    "parent directory",
			baseURL: "mark://host:6309/a/b/page.md",
			dest:    "../c.md",
			want:    "mark://host:6309/a/c.md",
		},
		{
			name:    "empty base URL",
			baseURL: "",
			dest:    "file.md",
			want:    "file.md",
		},
		{
			name:    "http link stays absolute",
			baseURL: "mark://host:6309/page.md",
			dest:    "https://example.com/doc",
			want:    "https://example.com/doc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.baseURL, tt.dest)
			if got != tt.want {
				t.Errorf("Resolve(%q, %q) = %q, want %q", tt.baseURL, tt.dest, got, tt.want)
			}
		})
	}
}

func TestExtractTitle(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "h1 heading",
			body: "# My Document\n\nSome content.",
			want: "My Document",
		},
		{
			name: "no heading",
			body: "Just plain text.",
			want: "",
		},
		{
			name: "h2 only",
			body: "## Not a title\n\nContent.",
			want: "",
		},
		{
			name: "h1 after h2",
			body: "## Sub\n\n# Main Title\n\nContent.",
			want: "Main Title",
		},
		{
			name: "heading with emphasis",
			body: "# Hello **world**\n\nContent.",
			want: "Hello world",
		},
		{
			name: "heading with code span",
			body: "# The `mark` Protocol\n\nContent.",
			want: "The mark Protocol",
		},
		{
			name: "empty body",
			body: "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractTitle(tt.body)
			if got != tt.want {
				t.Errorf("ExtractTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCanonicalURL(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"default port stripped", "mark://host:6309/x.md", "mark://host/x.md"},
		{"already canonical", "mark://host/x.md", "mark://host/x.md"},
		{"non-default port kept", "mark://host:7000/x.md", "mark://host:7000/x.md"},
		{"broker world untouched", "mark://servicing/x.md", "mark://servicing/x.md"},
		{"broker world with default port", "mark://servicing:6309/x.md", "mark://servicing/x.md"},
		{"empty path becomes root", "mark://host:6309", "mark://host/"},
		{"directory path kept", "mark://host:6309/plans/", "mark://host/plans/"},
		{"ipv6 default port", "mark://[::1]:6309/x.md", "mark://[::1]/x.md"},
		{"ipv6 non-default port", "mark://[::1]:7000/x.md", "mark://[::1]:7000/x.md"},
		{"external untouched", "https://example.com/x", "https://example.com/x"},
		{"relative untouched", "/plans/x.md", "/plans/x.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CanonicalURL(tt.in)
			if got != tt.want {
				t.Errorf("CanonicalURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if again := CanonicalURL(got); again != got {
				t.Errorf("not idempotent: CanonicalURL(%q) = %q", got, again)
			}
		})
	}
}

// NodeURL and CanonicalURL must not drift: one builds identity from parsed
// parts, the other normalizes a whole URL, and both encode the same rule.
func TestNodeURLMatchesCanonicalURL(t *testing.T) {
	cases := []struct{ host, path string }{
		{"host:6309", "/x.md"},
		{"host", "/x.md"},
		{"host:7000", "/x.md"},
		{"servicing", "/x.md"},
		{"host:6309", "/plans/"},
		{"[::1]:6309", "/x.md"},
		{"host:6309", ""},
	}
	for _, c := range cases {
		got := NodeURL(c.host, c.path)
		path := c.path
		if path == "" {
			path = "/"
		}
		if want := CanonicalURL("mark://" + c.host + path); got != want {
			t.Errorf("NodeURL(%q, %q) = %q, want %q", c.host, c.path, got, want)
		}
		if strings.HasSuffix(c.host, ":6309") && strings.Contains(got, ":6309") {
			t.Errorf("NodeURL(%q, %q) = %q, kept the default port", c.host, c.path, got)
		}
	}
}

// The destination fragment is kept beside the stripped Dest, for labeled
// and empty-text links alike.
func TestExtractWithPositionsKeepsTheFragment(t *testing.T) {
	infos := ExtractWithPositions("See [one](/t.md#one), [](/t.md#two) and [whole](/t.md).\n")
	want := []struct{ dest, fragment string }{{"/t.md", "one"}, {"/t.md", "two"}, {"/t.md", ""}}
	if len(infos) != len(want) {
		t.Fatalf("links = %+v, want %d", infos, len(want))
	}
	for i, w := range want {
		if infos[i].Dest != w.dest || infos[i].Fragment != w.fragment {
			t.Errorf("link %d = %+v, want dest %q fragment %q", i, infos[i], w.dest, w.fragment)
		}
	}
}

func TestResolvePath(t *testing.T) {
	for dest, want := range map[string]string{
		"/a/b.md": "/a/b.md", "b.md": "/docs/b.md", "../x.md": "/x.md", "sub/": "/docs/sub/", "/j/": "/j/",
		"b.md#sec": "/docs/b.md", "b.md?x=1": "/docs/b.md", "https://e.example/b.md": "", "": "",
		"mailto:user@example.org": "", "mark://w/x.md": "",
	} {
		if got := ResolvePath("/docs/a.md", dest); got != want {
			t.Errorf("ResolvePath(%q) = %q, want %q", dest, got, want)
		}
	}
}

func TestResolverPath(t *testing.T) {
	for host, cases := range map[string]map[string]string{
		"":               {"mark://w/x.md": "", "b.md": "/docs/b.md"},
		"w":              {"mark://w/x.md": "/x.md", "MARK://W/x.md": "/x.md", "mark://w:6309/d/": "/d/", "mark://w/x.md#s": "/x.md", "mark://v/x.md": "", "mark://w:6310/x.md": ""},
		"localhost:6310": {"mark://localhost:6310/x.md": "/x.md", "mark://localhost/x.md": ""},
	} {
		r := ResolverFor(host)
		for dest, want := range cases {
			if got := r.Path("/docs/a.md", dest); got != want {
				t.Errorf("ResolverFor(%q).Path(%q) = %q, want %q", host, dest, got, want)
			}
		}
	}
}

// Inline links and images and reference definitions are rewritable; code,
// escaped brackets and reference uses are not, and a span skips any title.
func TestSpans(t *testing.T) {
	body := "[a](one.md) [b](#two \"Two\") [c]( <three.md> ) ![i](p.png) `[d](four.md)` \\[e](five.md) [h][r] [](nine.md) [j](<a b.md>)\n\n```\n[g](seven.md)\n```\n\n[r]: eight.md \"T\"\n[s]:\n  <ten.md>\n"
	got := make([]string, 0, 8)
	for _, sp := range Spans(body) {
		span := "none"
		if sp.Rewritable() {
			span = body[sp.Start:sp.End]
		}
		got = append(got, sp.Dest+"="+span)
	}
	want := []string{"one.md=one.md", "#two=#two", "three.md=three.md", "p.png=p.png", "nine.md=none", "a b.md=a b.md", "eight.md=eight.md", "ten.md=ten.md"}
	if !slices.Equal(got, want) {
		t.Errorf("Spans = %q, want %q", got, want)
	}
}

// A definition's text is as written, a quote's prefixes dropped, so it reads
// the same copied out of its container.
func TestDefinitions(t *testing.T) {
	body := "[a]: one.md \"T\"\n\n> [b]:\n> <two.md>\n> 'Two'\n\n[A]: three.md\n"
	got := make([]string, 0, 3)
	for _, d := range Definitions(body) {
		got = append(got, fmt.Sprintf("%s@%d=%s", d.Label, d.Pos, d.Text))
	}
	want := []string{"a@0=[a]: one.md \"T\"", "b@19=[b]:\n<two.md>\n'Two'", "A@44=[A]: three.md"}
	if !slices.Equal(got, want) {
		t.Errorf("Definitions = %q, want %q", got, want)
	}
}

func TestTargets(t *testing.T) {
	body := "[a](#one) [b][r] [c] ![i](p.png) <mark://h/d.md#two> <a href=\"#three\">x</a> <img src='q.png'> `[d](#no)`\n\n<div><a href=/e.md#four>y</a></div>\n\nthen [e](#seven)\n\n[r]: /r.md#five\n[c]: #six\n"
	got := make([]string, 0, 8)
	for _, f := range Targets(body) {
		got = append(got, fmt.Sprintf("%s@%d[%s]", f.Dest, f.Pos, f.Label))
	}
	want := []string{"#one@0[]", "/r.md#five@0[r]", "#six@0[c]", "p.png@0[]", "mark://h/d.md#two@0[]", "#three@0[]", "q.png@0[]", "/e.md#four@106[]", "#seven@143[]"}
	if !slices.Equal(got, want) {
		t.Errorf("Targets = %q, want %q", got, want)
	}
}

// A destination names the document itself by fragment, query, path, or a
// mark:// URL on the resolver's store.
func TestSelf(t *testing.T) {
	r := ResolverFor("h")
	for dest, want := range map[string]bool{"": true, "?x": true, "/docs/d.md": true, "d.md": true, "../docs/d.md?x": true, "mark://h/docs/d.md": true, "mark://h:6309/docs/d.md?x": true, "/other.md": false, "mark://o/docs/d.md": false, "https://h/docs/d.md": false} {
		if got := r.Self("/docs/d.md", dest); got != want {
			t.Errorf("Self(%q) = %v, want %v", dest, got, want)
		}
	}
	if (Resolver{}).Self("/docs/d.md", "mark://h/docs/d.md") {
		t.Error("an unbound resolver reads a mark:// URL as the document")
	}
}
