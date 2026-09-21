package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

// A directory named like a version suffix is a directory, not version N of its parent.
func TestFetchDirectoryNamedLikeVersion(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newBackend backendFactory) {
		b := newBackend(t)
		seedBackend(t, b, map[string]string{
			"api/v2/guide.md": "# Guide\n",
			"api/v3/index.md": "# V3 home\n",
			"doc.md":          "# Doc\n",
		})
		h := newHandler(b, nil)

		tests := []struct {
			name     string
			request  string
			status   string
			contains string
		}{
			{name: "listing", request: "FETCH /api/v2\n", status: protocol.StatusOK, contains: "guide.md"},
			{name: "index document", request: "FETCH /api/v3\n", status: protocol.StatusOK, contains: "# V3 home"},
			{name: "real version still resolves", request: "FETCH /doc.md/v1\n", status: protocol.StatusOK, contains: "# Doc"},
			{name: "missing version still not found", request: "FETCH /doc.md/v9\n", status: protocol.StatusNotFound},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				stream := newMockStream(tt.request)
				h.HandleStream(context.Background(), stream)
				resp, err := protocol.ParseResponse(&stream.output)
				if err != nil {
					t.Fatalf("parse response: %v", err)
				}
				if resp.Status != tt.status {
					t.Fatalf("status = %q, want %q (body %q)", resp.Status, tt.status, resp.Body)
				}
				if !strings.Contains(resp.Body, tt.contains) {
					t.Errorf("body %q does not contain %q", resp.Body, tt.contains)
				}
			})
		}
	})
}
