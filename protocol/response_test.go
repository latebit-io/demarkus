package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseResponse(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantStatus string
		wantMeta   map[string]string
		wantBody   string
		wantErr    bool
	}{
		{
			name: "ok response with metadata",
			input: "---\nstatus: ok\nmodified: 2025-02-14T10:30:00Z\nversion: 42\n---\n" +
				"# Hello\n",
			wantStatus: "ok",
			wantMeta:   map[string]string{"modified": "2025-02-14T10:30:00Z", "version": "42"},
			wantBody:   "# Hello\n",
		},
		{
			name:       "not-found response",
			input:      "---\nstatus: not-found\n---\n# Not Found\n",
			wantStatus: "not-found",
			wantMeta:   map[string]string{},
			wantBody:   "# Not Found\n",
		},
		{
			name:       "no frontmatter",
			input:      "# Just markdown\n",
			wantStatus: "",
			wantMeta:   map[string]string{},
			wantBody:   "# Just markdown\n",
		},
		{
			name:    "unclosed frontmatter",
			input:   "---\nstatus: ok\n# No closing\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseResponse(strings.NewReader(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Status != tt.wantStatus {
				t.Errorf("status: got %q, want %q", got.Status, tt.wantStatus)
			}
			if got.Body != tt.wantBody {
				t.Errorf("body: got %q, want %q", got.Body, tt.wantBody)
			}
			for k, want := range tt.wantMeta {
				if got.Metadata[k] != want {
					t.Errorf("metadata[%s]: got %q, want %q", k, got.Metadata[k], want)
				}
			}
			if len(got.Metadata) != len(tt.wantMeta) {
				t.Errorf("metadata length: got %d, want %d", len(got.Metadata), len(tt.wantMeta))
			}
		})
	}
}

func TestResponseWriteTo(t *testing.T) {
	resp := Response{
		Status:   StatusOK,
		Metadata: map[string]string{"modified": "2025-02-14T10:30:00Z", "version": "1"},
		Body:     "# Hello\n",
	}

	var buf bytes.Buffer
	if _, err := resp.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	got := buf.String()
	if !strings.HasPrefix(got, "---\n") {
		t.Errorf("should start with frontmatter delimiter, got: %q", got[:40])
	}
	if !strings.Contains(got, "status: ok") {
		t.Error("missing status")
	}
	if !strings.Contains(got, "modified:") || !strings.Contains(got, "2025-02-14T10:30:00Z") {
		t.Error("missing modified metadata")
	}
	if !strings.Contains(got, "version:") || !strings.Contains(got, "1") {
		t.Error("missing version metadata")
	}
	if !strings.HasSuffix(got, "# Hello\n") {
		t.Errorf("should end with body, got: %q", got[len(got)-20:])
	}
}

func TestResponseRoundTrip(t *testing.T) {
	original := Response{
		Status:   StatusOK,
		Metadata: map[string]string{"modified": "2025-02-14T10:30:00Z", "version": "42"},
		Body:     "# Test Document\n\nSome content here.\n",
	}

	var buf bytes.Buffer
	if _, err := original.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	parsed, err := ParseResponse(&buf)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}

	if parsed.Status != original.Status {
		t.Errorf("status: got %q, want %q", parsed.Status, original.Status)
	}
	if parsed.Body != original.Body {
		t.Errorf("body: got %q, want %q", parsed.Body, original.Body)
	}
	for k, want := range original.Metadata {
		if parsed.Metadata[k] != want {
			t.Errorf("metadata[%s]: got %q, want %q", k, parsed.Metadata[k], want)
		}
	}
}
