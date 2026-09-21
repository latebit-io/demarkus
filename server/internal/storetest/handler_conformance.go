package storetest

import (
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
)

// RunHandlerConformance pins wire behavior that depends on what the backend
// reports: path resolution around version shaped names and refusal statuses.
func RunHandlerConformance(t *testing.T, factory LookupFactory) {
	subtests := []struct {
		name string
		fn   func(t *testing.T, b LookupBackend)
	}{
		{"VersionNamedDirectory", testHandlerVersionNamedDirectory},
		{"PathCollisionIsBadRequest", testHandlerPathCollision},
	}
	for _, st := range subtests {
		t.Run(st.name, func(t *testing.T) { st.fn(t, factory(t)) })
	}
}

func testHandlerVersionNamedDirectory(t *testing.T, b LookupBackend) {
	h := NewHandler(b)
	publishDoc(t, h, "/api/v2/doc.md", "# Doc\n", nil)
	tests := []struct {
		name, path, wantStatus, wantBody string
	}{
		{"directory named like a version", "/api/v2", protocol.StatusOK, "[doc.md](doc.md)"},
		{"document under it", "/api/v2/doc.md", protocol.StatusOK, "# Doc"},
		{"version of that document", "/api/v2/doc.md/v1", protocol.StatusOK, "# Doc"},
		{"no such version or directory", "/api/v9", protocol.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := Send(t, h, request(protocol.VerbFetch, tt.path, nil, ""))
			if resp.Status != tt.wantStatus || !strings.Contains(resp.Body, tt.wantBody) {
				t.Errorf("FETCH %s = %s %q, want %s containing %q", tt.path, resp.Status, resp.Body, tt.wantStatus, tt.wantBody)
			}
		})
	}
}

func testHandlerPathCollision(t *testing.T, b LookupBackend) {
	h := NewHandler(b)
	publishDoc(t, h, "/a.md", "# A\n", nil)
	resp := Send(t, h, request(protocol.VerbPublish, "/a.md/b.md", map[string]string{"expected-version": "0"}, "# B\n"))
	if resp.Status != protocol.StatusBadRequest {
		t.Errorf("publish beneath a document = %s, want %s", resp.Status, protocol.StatusBadRequest)
	}
}

// RejectionFactories open backends that refuse writes a client can correct.
type RejectionFactories struct {
	// Quota caps the backend at two documents; nil for a backend without quotas.
	Quota LookupFactory
	// Policy blocks a publish that lacks a domain tag.
	Policy LookupFactory
}

// RunRejectionConformance holds a backend that enforces a quota or a publish
// policy to the shared sentinels and to the statuses the handler maps them to.
func RunRejectionConformance(t *testing.T, factories RejectionFactories) {
	if factories.Quota != nil {
		t.Run("QuotaIsNotPermitted", func(t *testing.T) { testQuotaRejection(t, factories.Quota(t)) })
	}
	t.Run("PolicyIsBadRequest", func(t *testing.T) { testPolicyRejection(t, factories.Policy(t)) })
}

func testQuotaRejection(t *testing.T, b LookupBackend) {
	h := NewHandler(b)
	publishDoc(t, h, "/a.md", "# A\n", nil)
	publishDoc(t, h, "/b.md", "# B\n", nil)

	_, err := b.direct().WriteVersion("/c.md", 0, []byte("# C\n"), nil)
	if !errors.Is(err, storagebackend.ErrQuota) {
		t.Fatalf("third document err = %v, want backend.ErrQuota", err)
	}
	over := request(protocol.VerbPublish, "/c.md", map[string]string{"expected-version": "0"}, "# C\n")
	if resp := Send(t, h, over); resp.Status != protocol.StatusNotPermitted {
		t.Errorf("publish past the quota = %s, want %s", resp.Status, protocol.StatusNotPermitted)
	}
	// The quota counts documents, so an existing one still takes versions.
	next := request(protocol.VerbPublish, "/a.md", map[string]string{"expected-version": "1"}, "# A2\n")
	if resp := Send(t, h, next); resp.Status != protocol.StatusCreated {
		t.Errorf("new version at the quota = %s, want %s", resp.Status, protocol.StatusCreated)
	}
}

func testPolicyRejection(t *testing.T, b LookupBackend) {
	_, err := b.direct().WriteVersion("/untagged.md", 0, []byte("# Untagged\n"), nil)
	if !errors.Is(err, storagebackend.ErrRejected) {
		t.Fatalf("untagged publish err = %v, want backend.ErrRejected", err)
	}
	var rejection storagebackend.Rejection
	if !errors.As(err, &rejection) || rejection.RejectionMessage() == "" {
		t.Fatalf("rejection %v carries no message for the client", err)
	}

	h := NewHandler(b)
	blocked := request(protocol.VerbPublish, "/untagged.md", map[string]string{"expected-version": "0"}, "# Untagged\n")
	resp := Send(t, h, blocked)
	if resp.Status != protocol.StatusBadRequest || !strings.Contains(resp.Body, rejection.RejectionMessage()) {
		t.Errorf("blocked publish = %s %q, want %s naming %q", resp.Status, resp.Body, protocol.StatusBadRequest, rejection.RejectionMessage())
	}
	tagged := request(protocol.VerbPublish, "/tagged.md", map[string]string{"expected-version": "0", "tags": "domain:test"}, "# Tagged\n")
	if resp := Send(t, h, tagged); resp.Status != protocol.StatusCreated {
		t.Errorf("tagged publish = %s, want %s", resp.Status, protocol.StatusCreated)
	}
}
