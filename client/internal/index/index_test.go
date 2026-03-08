package index

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []Entry
	}{
		{
			name: "valid table",
			body: `# Content Index

> Source: mark://docs.example.com
> Indexed: 2026-03-07T15:30:00Z
> Documents: 2

| Hash | Server | Path |
|------|--------|------|
| sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2 | mark://docs.example.com | /guide.md |
| sha256-7890abcd7890abcd7890abcd7890abcd7890abcd7890abcd7890abcd7890abcd | mark://docs.example.com | /api/ref.md |
`,
			want: []Entry{
				{Hash: "sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", Server: "mark://docs.example.com", Path: "/guide.md"},
				{Hash: "sha256-7890abcd7890abcd7890abcd7890abcd7890abcd7890abcd7890abcd7890abcd", Server: "mark://docs.example.com", Path: "/api/ref.md"},
			},
		},
		{
			name: "empty body",
			body: "",
			want: nil,
		},
		{
			name: "no table",
			body: "# Just a heading\n\nSome text.\n",
			want: nil,
		},
		{
			name: "table with only header",
			body: "| Hash | Server | Path |\n|------|--------|------|\n",
			want: nil,
		},
		{
			name: "malformed row skipped",
			body: `| Hash | Server | Path |
|------|--------|------|
| sha256-aaaa | mark://x.com | /a.md |
| not a valid row |
| sha256-bbbb | mark://y.com | /b.md |
`,
			want: []Entry{
				{Hash: "sha256-aaaa", Server: "mark://x.com", Path: "/a.md"},
				{Hash: "sha256-bbbb", Server: "mark://y.com", Path: "/b.md"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.body)
			if len(got) != len(tt.want) {
				t.Fatalf("Parse() returned %d entries, want %d", len(got), len(tt.want))
			}
			for i, g := range got {
				w := tt.want[i]
				if g.Hash != w.Hash || g.Server != w.Server || g.Path != w.Path {
					t.Errorf("entry[%d] = %+v, want %+v", i, g, w)
				}
			}
		})
	}
}

func TestBuild(t *testing.T) {
	entries := []Entry{
		{Hash: "sha256-aaa", Server: "mark://a.com", Path: "/one.md"},
		{Hash: "sha256-bbb", Server: "mark://b.com", Path: "/two.md"},
	}
	indexed := time.Date(2026, 3, 7, 15, 30, 0, 0, time.UTC)
	body := Build("mark://a.com", indexed, entries)

	if !strings.Contains(body, "> Source: mark://a.com") {
		t.Error("missing source in header")
	}
	if !strings.Contains(body, "> Indexed: 2026-03-07T15:30:00Z") {
		t.Error("missing indexed timestamp in header")
	}
	if !strings.Contains(body, "> Documents: 2") {
		t.Error("missing document count in header")
	}
	if !strings.Contains(body, "| sha256-aaa | mark://a.com | /one.md |") {
		t.Error("missing first entry")
	}
	if !strings.Contains(body, "| sha256-bbb | mark://b.com | /two.md |") {
		t.Error("missing second entry")
	}
}

func TestBuildRoundTrip(t *testing.T) {
	entries := []Entry{
		{Hash: "sha256-aaa", Server: "mark://a.com", Path: "/one.md"},
		{Hash: "sha256-bbb", Server: "mark://b.com", Path: "/two.md"},
	}
	indexed := time.Date(2026, 3, 7, 15, 30, 0, 0, time.UTC)
	body := Build("mark://a.com", indexed, entries)
	got := Parse(body)

	if len(got) != len(entries) {
		t.Fatalf("round-trip: got %d entries, want %d", len(got), len(entries))
	}
	for i, g := range got {
		w := entries[i]
		if g.Hash != w.Hash || g.Server != w.Server || g.Path != w.Path {
			t.Errorf("round-trip entry[%d] = %+v, want %+v", i, g, w)
		}
	}
}

func TestMerge(t *testing.T) {
	tests := []struct {
		name         string
		existing     []Entry
		sourceServer string
		newEntries   []Entry
		want         []Entry
	}{
		{
			name: "replace source entries keep others",
			existing: []Entry{
				{Hash: "sha256-aaa", Server: "mark://a.com", Path: "/old.md"},
				{Hash: "sha256-bbb", Server: "mark://b.com", Path: "/keep.md"},
				{Hash: "sha256-ccc", Server: "mark://a.com", Path: "/also-old.md"},
			},
			sourceServer: "mark://a.com",
			newEntries: []Entry{
				{Hash: "sha256-ddd", Server: "mark://a.com", Path: "/new.md"},
			},
			want: []Entry{
				{Hash: "sha256-bbb", Server: "mark://b.com", Path: "/keep.md"},
				{Hash: "sha256-ddd", Server: "mark://a.com", Path: "/new.md"},
			},
		},
		{
			name:         "empty existing",
			existing:     nil,
			sourceServer: "mark://a.com",
			newEntries: []Entry{
				{Hash: "sha256-aaa", Server: "mark://a.com", Path: "/new.md"},
			},
			want: []Entry{
				{Hash: "sha256-aaa", Server: "mark://a.com", Path: "/new.md"},
			},
		},
		{
			name: "remove all from source no new entries",
			existing: []Entry{
				{Hash: "sha256-aaa", Server: "mark://a.com", Path: "/old.md"},
			},
			sourceServer: "mark://a.com",
			newEntries:   nil,
			want:         nil,
		},
		{
			name: "normalizes trailing slash",
			existing: []Entry{
				{Hash: "sha256-aaa", Server: "mark://a.com/", Path: "/old.md"},
			},
			sourceServer: "mark://a.com",
			newEntries: []Entry{
				{Hash: "sha256-bbb", Server: "mark://a.com", Path: "/new.md"},
			},
			want: []Entry{
				{Hash: "sha256-bbb", Server: "mark://a.com", Path: "/new.md"},
			},
		},
		{
			name: "normalizes default port",
			existing: []Entry{
				{Hash: "sha256-aaa", Server: "mark://a.com:6309", Path: "/old.md"},
			},
			sourceServer: "mark://a.com",
			newEntries: []Entry{
				{Hash: "sha256-bbb", Server: "mark://a.com", Path: "/new.md"},
			},
			want: []Entry{
				{Hash: "sha256-bbb", Server: "mark://a.com", Path: "/new.md"},
			},
		},
		{
			name: "normalizes case",
			existing: []Entry{
				{Hash: "sha256-aaa", Server: "mark://A.COM", Path: "/old.md"},
			},
			sourceServer: "mark://a.com",
			newEntries: []Entry{
				{Hash: "sha256-bbb", Server: "mark://a.com", Path: "/new.md"},
			},
			want: []Entry{
				{Hash: "sha256-bbb", Server: "mark://a.com", Path: "/new.md"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Merge(tt.existing, tt.sourceServer, tt.newEntries)
			if len(got) != len(tt.want) {
				t.Fatalf("Merge() returned %d entries, want %d", len(got), len(tt.want))
			}
			for i, g := range got {
				w := tt.want[i]
				if g.Hash != w.Hash || g.Server != w.Server || g.Path != w.Path {
					t.Errorf("entry[%d] = %+v, want %+v", i, g, w)
				}
			}
		})
	}
}
