package links

import (
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
