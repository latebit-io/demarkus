package links

import "testing"

func TestParseMark(t *testing.T) {
	tests := []struct {
		name, raw                   string
		dial, authority, node, path string
	}{
		{"default port filled for dial only", "mark://world/Index.md", "world:6309", "mark://world", "mark://world/Index.md", "/Index.md"},
		{"default port stripped from identity", "mark://world:6309/x.md", "world:6309", "mark://world", "mark://world/x.md", "/x.md"},
		{"other port kept", "mark://world:7000", "world:7000", "mark://world:7000", "mark://world:7000/", "/"},
		{"ipv6", "mark://[2001:db8::1]/index.md", "[2001:db8::1]:6309", "mark://[2001:db8::1]", "mark://[2001:db8::1]/index.md", "/index.md"},
		{"fragment and query dropped", "mark://team-a/foo.md?rev=2#section", "team-a:6309", "mark://team-a", "mark://team-a/foo.md", "/foo.md"},
		{"userinfo dropped", "mark://user@world/x.md", "world:6309", "mark://world", "mark://world/x.md", "/x.md"},
		{"escaped path", "mark://world/a%20b.md", "world:6309", "mark://world", "mark://world/a%20b.md", "/a b.md"},
		{"directory", "mark://world:6309/plans/", "world:6309", "mark://world", "mark://world/plans/", "/plans/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := ParseMark(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := target.DialHost(); got != tt.dial {
				t.Errorf("DialHost = %q, want %q", got, tt.dial)
			}
			if got := target.AuthorityURL(); got != tt.authority {
				t.Errorf("AuthorityURL = %q, want %q", got, tt.authority)
			}
			if got := target.NodeURL(); got != tt.node {
				t.Errorf("NodeURL = %q, want %q", got, tt.node)
			}
			if target.Path != tt.path {
				t.Errorf("Path = %q, want %q", target.Path, tt.path)
			}
			if got := CanonicalURL(tt.raw); got != tt.node {
				t.Errorf("CanonicalURL = %q, want NodeURL %q", got, tt.node)
			}
		})
	}
}

// Host case never names a different server (ADR 0018): dial address and
// identity both lowercase it; the path keeps its case.
func TestParseMarkLowercasesHost(t *testing.T) {
	target, err := ParseMark("mark://WORLD.Example:6309/Index.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := target.DialHost(); got != "world.example:6309" {
		t.Errorf("DialHost = %q, want world.example:6309", got)
	}
	if got := target.Hostname(); got != "world.example" {
		t.Errorf("Hostname = %q, want it lowercase: host case is not identity", got)
	}
	if got := target.AuthorityURL(); got != "mark://world.example" {
		t.Errorf("AuthorityURL = %q, want mark://world.example", got)
	}
	if got := target.NodeURL(); got != "mark://world.example/Index.md" {
		t.Errorf("NodeURL = %q, want mark://world.example/Index.md", got)
	}
	for _, spelling := range []string{"mark://World.Example/Index.md", "mark://world.example:6309/Index.md"} {
		if got := CanonicalURL(spelling); got != target.NodeURL() {
			t.Errorf("CanonicalURL(%q) = %q, want %q", spelling, got, target.NodeURL())
		}
	}
	if got := AuthorityURL("HOST:6309"); got != "mark://host" {
		t.Errorf("AuthorityURL(HOST:6309) = %q, want mark://host", got)
	}
}

// The broker routes Hostname as a world name: as written, never with a port.
func TestParseMarkHostname(t *testing.T) {
	for raw, want := range map[string]string{
		"mark://team-a":               "team-a",
		"mark://team-a:6309/foo.md#s": "team-a",
		"mark://[::1]:7000/x.md":      "::1",
	} {
		target, err := ParseMark(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if got := target.Hostname(); got != want {
			t.Errorf("%q: Hostname = %q, want %q", raw, got, want)
		}
	}
}

func TestParseMarkRejects(t *testing.T) {
	for _, raw := range []string{
		"", "mark://", "mark:///foo", "https://example.com/foo", "/plans/x.md",
		"mark://world:0", "mark://world:65536", "mark://world:abc/x",
	} {
		t.Run(raw, func(t *testing.T) {
			if target, err := ParseMark(raw); err == nil {
				t.Fatalf("ParseMark(%q) = %+v, want an error", raw, target)
			}
		})
	}
}

func TestAuthorityURL(t *testing.T) {
	tests := []struct{ host, want string }{
		{"host:6309", "mark://host"},
		{"host", "mark://host"},
		{"host:7000", "mark://host:7000"},
		{"[::1]:6309", "mark://[::1]"},
		{"servicing", "mark://servicing"},
	}
	for _, tt := range tests {
		if got := AuthorityURL(tt.host); got != tt.want {
			t.Errorf("AuthorityURL(%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
}

// A server URL names an authority and nothing else: index rows, crawler seeds
// and hubs. Anything ParseMark would silently drop is refused here.
func TestParseServer(t *testing.T) {
	for raw, want := range map[string]string{
		"mark://host":          "mark://host",
		"mark://host/":         "mark://host",
		"mark://HOST:6309":     "mark://host",
		"mark://host:7000/":    "mark://host:7000",
		"mark://[::1]:6309":    "mark://[::1]",
		"mark://team-a":        "mark://team-a",
		"mark://docs.example/": "mark://docs.example",
	} {
		target, err := ParseServer(raw)
		if err != nil {
			t.Errorf("ParseServer(%q): %v", raw, err)
			continue
		}
		if got := target.AuthorityURL(); got != want {
			t.Errorf("ParseServer(%q).AuthorityURL() = %q, want %q", raw, got, want)
		}
	}
	for _, raw := range []string{
		"", "host:6309", "https://host", "mark://", "mark://host:0", "mark://host/doc.md",
		"mark://user@host", "mark://host?x=1", "mark://host#frag", "mark://host/?", "mark://ho\tst",
	} {
		if target, err := ParseServer(raw); err == nil {
			t.Errorf("ParseServer(%q) = %+v, want an error", raw, target)
		}
	}
}
