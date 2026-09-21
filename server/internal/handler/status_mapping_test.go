package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

// SPEC section 7: a malformed request is bad-request. Size limits stay
// server-error because sections 6.1 and 6.4 define them so.
func TestRequestErrorStatusMapping(t *testing.T) {
	oversized := "PUBLISH /a.md\n" + strings.Repeat("x", protocol.MaxBodyLength+1)
	tests := []struct {
		name    string
		request string
		want    string
	}{
		{name: "no path", request: "FETCH\n", want: protocol.StatusBadRequest},
		{name: "unknown verb", request: "DELETE /a.md\n", want: protocol.StatusBadRequest},
		{name: "empty verb", request: " /a.md\n", want: protocol.StatusBadRequest},
		{name: "relative path", request: "FETCH a.md\n", want: protocol.StatusBadRequest},
		{name: "unclosed frontmatter", request: "FETCH /a.md\n---\nkey: v\n", want: protocol.StatusBadRequest},
		{name: "metadata not a map", request: "FETCH /a.md\n---\n- a\n- b\n---\n", want: protocol.StatusBadRequest},
		{name: "oversized body", request: oversized, want: protocol.StatusServerError},
		{name: "truncated before newline", request: "", want: protocol.StatusServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := &mockStream{Reader: strings.NewReader(tt.request)}
			(&Handler{Logger: discardLogger}).HandleStream(context.Background(), stream)
			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp.Status != tt.want {
				t.Errorf("status = %q, want %q", resp.Status, tt.want)
			}
		})
	}
}
