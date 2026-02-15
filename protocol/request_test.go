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
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRequestWriteTo(t *testing.T) {
	req := Request{Verb: "FETCH", Path: "/hello.md"}
	var buf bytes.Buffer
	n, err := req.WriteTo(&buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != int64(buf.Len()) {
		t.Errorf("WriteTo returned %d, but wrote %d bytes", n, buf.Len())
	}

	want := "FETCH /hello.md\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
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

	if parsed != original {
		t.Errorf("round-trip failed: got %+v, want %+v", parsed, original)
	}
}
