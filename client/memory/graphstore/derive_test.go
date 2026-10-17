package graphstore

import (
	"reflect"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func TestDeriveSourceKeepsGraphEdgesOncePerTargetAndRelation(t *testing.T) {
	resp := &protocol.Response{
		Status: protocol.StatusOK,
		Body: "# Fallback\n\n## Part\n\n[first](b.md) [](b.md) [second](/b.md)\n" +
			"[ext](mark://ext.example.com:6309/x.md) [web](https://example.com/) " +
			"[loop](mark://127.0.0.1:6401/x.md) [local](mark://localhost/y.md)\n",
		Metadata: map[string]string{"version": "4", "etag": "e4", "rel-supersedes": "/a.md", "rel-cites": "/docs/a.md"},
	}
	got, rejected := DeriveSource("team:6309", "/docs/a.md", resp)
	want := WorldSource{Path: "/docs/a.md", Version: 4, Etag: "e4", Title: "Fallback", Edges: []WorldEdge{
		{To: "mark://ext.example.com/x.md", Label: "ext", Anchor: "part", Count: 1},
		{To: "mark://team/a.md", Rel: "supersedes", Count: 1},
		{To: "mark://team/b.md", Label: "second", Anchor: "part", Count: 1},
		{To: "mark://team/docs/b.md", Label: "first", Anchor: "part", Count: 2},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("source =\n%+v\nwant\n%+v", got, want)
	}
	if len(rejected) != 1 || rejected[0].Reason != "self reference" {
		t.Errorf("rejected = %+v, want the self reference", rejected)
	}
	if got.LinkCount() != 4 {
		t.Errorf("link count = %d, want the 4 body links kept", got.LinkCount())
	}
}

func TestDeriveSourceTitles(t *testing.T) {
	for _, test := range []struct {
		name, title, body, want string
	}{
		{name: "metadata wins", title: "Meta", body: "# Heading\n", want: "Meta"},
		{name: "heading fallback", body: "# Heading\n", want: "Heading"},
		{name: "none", body: "text\n", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			meta := map[string]string{"version": "1"}
			if test.title != "" {
				meta["title"] = test.title
			}
			got, _ := DeriveSource("team", "/a.md", &protocol.Response{Body: test.body, Metadata: meta})
			if got.Title != test.want {
				t.Errorf("title = %q, want %q", got.Title, test.want)
			}
		})
	}
	if got, _ := DeriveSource("team", "/a.md", &protocol.Response{Metadata: map[string]string{"version": "x", "title": "a\nb"}}); got.Version != 0 || got.Title != "" {
		t.Errorf("bad version and title = %d %q, want 0 and none", got.Version, got.Title)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:6401":        true,  // IPv4 loopback + port
		"localhost":             true,  // bare localhost
		"localhost:6309":        true,  // localhost + port
		"0.0.0.0:6309":          true,  // unspecified
		"[::1]:6309":            true,  // bracketed IPv6 loopback + port
		"::1":                   true,  // bare IPv6 loopback
		"::1:6309":              true,  // unbracketed IPv6 loopback + port
		"soul.demarkus.io:6309": false, // real external world
		"10.0.0.5:6309":         false, // private IP: a real LAN or cluster world, kept
		"2001:db8::5":           false, // routable IPv6
	}
	for in, want := range cases {
		if got := isLoopbackHost(in); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", in, got, want)
		}
	}
}
