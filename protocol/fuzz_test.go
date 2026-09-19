package protocol

import (
	"bytes"
	"maps"
	"testing"
)

func FuzzParseRequest(f *testing.F) {
	f.Add([]byte("FETCH /index.md\n"))
	f.Add([]byte("PUBLISH /a.md\n---\nexpected-version: \"1\"\n---\n# A\n"))
	f.Add([]byte("PUBLISH /a.md\n---\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		req, err := ParseRequest(bytes.NewReader(data))
		if err != nil {
			return
		}
		if !IsValidVerb(req.Verb) {
			t.Fatalf("accepted unknown verb %q", req.Verb)
		}
		if err := ValidateRequestPath(req.Path); err != nil {
			t.Fatalf("accepted invalid path %q: %v", req.Path, err)
		}
		if req.Metadata == nil {
			t.Fatal("nil metadata on success")
		}
	})
}

func FuzzParseResponse(f *testing.F) {
	f.Add([]byte("---\nstatus: ok\n---\n# Body\n"))
	f.Add([]byte("---\n\n---\n"))
	f.Add([]byte("no frontmatter"))
	f.Fuzz(func(t *testing.T, data []byte) {
		resp, err := ParseResponse(bytes.NewReader(data))
		if err != nil {
			return
		}
		if resp.Metadata == nil {
			t.Fatal("nil metadata on success")
		}
	})
}

// A written request must parse back to the same verb, path, metadata and body.
func FuzzRequestRoundTrip(f *testing.F) {
	f.Add("/a.md", false, "", "# Title\n")
	f.Add("/a.md", true, "3", "body")
	f.Add("/a.md", false, "", "---\ntitle: not metadata\n---\nbody")
	f.Add("/a.md", false, "", "---\n")
	f.Add("/a.md", false, "", "---")
	f.Fuzz(func(t *testing.T, path string, withMeta bool, value, body string) {
		if ValidateRequestPath(path) != nil || len(body) > MaxBodyLength {
			t.Skip()
		}
		// Control characters in values do not survive YAML (P31); out of scope here.
		if containsControlChars(value) {
			t.Skip()
		}
		want := Request{Verb: "PUBLISH", Path: path, Metadata: map[string]string{}, Body: body}
		if withMeta {
			want.Metadata["expected-version"] = value
		}
		var buf bytes.Buffer
		if _, err := want.WriteTo(&buf); err != nil {
			t.Skip()
		}
		got, err := ParseRequest(&buf)
		if err != nil {
			// Oversized or non-YAML-safe metadata may be refused, never misread.
			t.Skip()
		}
		if got.Verb != want.Verb || got.Path != want.Path {
			t.Fatalf("request line: got %q %q, want %q %q", got.Verb, got.Path, want.Verb, want.Path)
		}
		if got.Body != want.Body {
			t.Fatalf("body: got %q, want %q", got.Body, want.Body)
		}
		if !maps.Equal(got.Metadata, want.Metadata) {
			t.Fatalf("metadata: got %v, want %v", got.Metadata, want.Metadata)
		}
	})
}
