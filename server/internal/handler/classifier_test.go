package handler

import (
	"context"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
)

// The classifier verdict is server-owned: a writer's own is never stored.
func TestClassifierKeyIsDropped(t *testing.T) { forEachBackend(t, testClassifierKeyIsDropped) }

func testClassifierKeyIsDropped(t *testing.T, newBackend backendFactory) {
	ts := auth.NewTokenStore(map[string]auth.Token{protocol.HashToken("w"): {Paths: []string{"/**"}, Operations: []string{"publish"}}})
	h := mustNew(Config{Store: newBackend(t).Store, Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }})
	do := func(request string) protocol.Response {
		t.Helper()
		stream := newMockStream(request)
		h.HandleStream(context.Background(), stream)
		resp, err := protocol.ParseResponse(&stream.output)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return resp
	}
	if got := do("PUBLISH /b.md\n---\nauth: w\nclassifier: benign\n---\n# B\n\nplain notes\n"); got.Status != protocol.StatusCreated {
		t.Fatalf("publish = %s %s", got.Status, got.Body)
	}
	if got := do("FETCH /b.md\n").Metadata; got[protocol.ClassifierKey] != "" {
		t.Errorf("forged verdict stored: %v", got)
	}
}
