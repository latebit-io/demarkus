package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseRequest(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Request
		wantErr bool
	}{
		{
			name:  "basic fetch",
			input: "FETCH /index.md\n",
			want:  Request{Verb: "FETCH", Path: "/index.md"},
		},
		{
			name:  "path with subdirectory",
			input: "FETCH /docs/article.md\n",
			want:  Request{Verb: "FETCH", Path: "/docs/article.md"},
		},
		{
			name:    "empty input",
			input:   "",
			wantErr: true,
		},
		{
			name:    "no space separator",
			input:   "FETCH\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRequest(strings.NewReader(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Verb != tt.want.Verb {
				t.Errorf("verb: got %q, want %q", got.Verb, tt.want.Verb)
			}
			if got.Path != tt.want.Path {
				t.Errorf("path: got %q, want %q", got.Path, tt.want.Path)
			}
		})
	}
}

func TestParseRequestWithMetadata(t *testing.T) {
	input := "FETCH /index.md\n---\nif-modified-since: 2025-02-14T10:30:00Z\nif-none-match: abc123\n---\n"

	req, err := ParseRequest(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Verb != "FETCH" {
		t.Errorf("verb: got %q, want %q", req.Verb, "FETCH")
	}
	if req.Path != "/index.md" {
		t.Errorf("path: got %q, want %q", req.Path, "/index.md")
	}
	if req.Metadata["if-modified-since"] != "2025-02-14T10:30:00Z" {
		t.Errorf("if-modified-since: got %q", req.Metadata["if-modified-since"])
	}
	if req.Metadata["if-none-match"] != "abc123" {
		t.Errorf("if-none-match: got %q", req.Metadata["if-none-match"])
	}
}

func TestParseRequestNoMetadata(t *testing.T) {
	input := "FETCH /index.md\n"

	req, err := ParseRequest(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Metadata) != 0 {
		t.Errorf("expected empty metadata, got %v", req.Metadata)
	}
}

func TestRequestWriteTo(t *testing.T) {
	req := Request{Verb: "FETCH", Path: "/hello.md"}
	var buf bytes.Buffer
	_, err := req.WriteTo(&buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "FETCH /hello.md\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestRequestWriteToWithMetadata(t *testing.T) {
	req := Request{
		Verb: "FETCH",
		Path: "/index.md",
		Metadata: map[string]string{
			"if-none-match":     "abc123",
			"if-modified-since": "2025-02-14T10:30:00Z",
		},
	}
	var buf bytes.Buffer
	_, err := req.WriteTo(&buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := buf.String()
	if !strings.HasPrefix(got, "FETCH /index.md\n---\n") {
		t.Errorf("missing verb line + frontmatter start: %q", got)
	}
	if !strings.Contains(got, "if-none-match: abc123\n") {
		t.Errorf("missing if-none-match: %q", got)
	}
	if !strings.Contains(got, "if-modified-since:") || !strings.Contains(got, "2025-02-14T10:30:00Z") {
		t.Errorf("missing if-modified-since: %q", got)
	}
	if !strings.HasSuffix(got, "---\n") {
		t.Errorf("missing closing ---: %q", got)
	}
}

func TestParseRequestScannerErrorInFrontmatter(t *testing.T) {
	// Frontmatter opened but never closed, with a line exceeding the scanner buffer.
	// This triggers a scanner error (token too long) inside the frontmatter loop.
	input := "FETCH /index.md\n---\n" + strings.Repeat("x", MaxRequestLineLength+1) + "\n"

	_, err := ParseRequest(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for scanner failure in frontmatter, got nil")
	}
	if !strings.Contains(err.Error(), "reading request metadata") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRequestRoundTrip(t *testing.T) {
	original := Request{Verb: "FETCH", Path: "/docs/test.md"}

	var buf bytes.Buffer
	if _, err := original.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	parsed, err := ParseRequest(&buf)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	if parsed.Verb != original.Verb || parsed.Path != original.Path {
		t.Errorf("round-trip failed: got %+v, want %+v", parsed, original)
	}
}

func TestRequestRoundTripWithMetadata(t *testing.T) {
	original := Request{
		Verb: "FETCH",
		Path: "/index.md",
		Metadata: map[string]string{
			"if-modified-since": "2025-02-14T10:30:00Z",
			"if-none-match":     "abc123",
		},
	}

	var buf bytes.Buffer
	if _, err := original.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	parsed, err := ParseRequest(&buf)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	if parsed.Verb != original.Verb {
		t.Errorf("verb: got %q, want %q", parsed.Verb, original.Verb)
	}
	if parsed.Path != original.Path {
		t.Errorf("path: got %q, want %q", parsed.Path, original.Path)
	}
	if parsed.Metadata["if-modified-since"] != original.Metadata["if-modified-since"] {
		t.Errorf("if-modified-since: got %q, want %q", parsed.Metadata["if-modified-since"], original.Metadata["if-modified-since"])
	}
	if parsed.Metadata["if-none-match"] != original.Metadata["if-none-match"] {
		t.Errorf("if-none-match: got %q, want %q", parsed.Metadata["if-none-match"], original.Metadata["if-none-match"])
	}
}
