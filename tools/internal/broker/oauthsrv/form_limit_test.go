package oauthsrv

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/internal/broker/brokertest"
	"k8s.io/client-go/kubernetes/fake"
)

// Unauthenticated form routes must refuse an oversized body instead of buffering it.
func TestFormRoutesCapBodySize(t *testing.T) {
	srv, _ := newTestServer(t, deviceTestConfig(), &brokertest.FakeVerifier{}, fake.NewSimpleClientset())
	oversized := url.Values{"client_id": {strings.Repeat("a", maxFormBytes+1)}}.Encode()

	for _, route := range []string{"/device/authorize", "/device", "/device/token", "/token/revoke"} {
		t.Run(route, func(t *testing.T) {
			resp, err := testClient(srv).Post(srv.URL+route, "application/x-www-form-urlencoded", strings.NewReader(oversized))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for a body over %d bytes", resp.StatusCode, maxFormBytes)
			}
		})
	}
}
