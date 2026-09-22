package oauthsrv

import (
	"net/http/httptest"
	"testing"

	"github.com/latebit-io/demarkus/tools/internal/broker/brokertest"
	"k8s.io/client-go/kubernetes/fake"
)

// TestRequireAuthEnforcesAllowDomains is TestGatewayAuthEnforcesAllowDomains
// for the management API's gate: same identities admitted and refused.
func TestRequireAuthEnforcesAllowDomains(t *testing.T) {
	for _, tt := range brokertest.AllowDomainCases() {
		t.Run(tt.Name, func(t *testing.T) {
			cfg := brokertest.NewConfig()
			cfg.Server.PublicURL = "https://broker.example.com"
			cfg.OIDC.AllowDomains = []string{"latebit.io"}
			idTokenSigner := brokertest.NewTestIDTokenSigner(t)
			_, srv := newTestServerWithSigner(t, cfg, brokertest.AllowDomainsVerifier(tt.HD), fake.NewSimpleClientset(), idTokenSigner)

			rec := httptest.NewRecorder()
			raw := brokertest.IdPBearer
			if tt.BrokerSigned {
				raw = brokertest.BrokerBearer(t, idTokenSigner, cfg.Server.PublicURL, tt.HD)
			}
			srv.requireAuth(brokertest.NoContent()).ServeHTTP(rec, brokertest.BearerRequest(raw))
			if rec.Code != tt.WantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.WantStatus)
			}
			if challenge := rec.Header().Get("WWW-Authenticate"); challenge != "" {
				t.Errorf("WWW-Authenticate = %q, want empty", challenge)
			}
		})
	}
}
