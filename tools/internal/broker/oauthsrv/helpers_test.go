package oauthsrv

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/latebit-io/demarkus/tools/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/tools/internal/broker/core"
	"github.com/latebit-io/demarkus/tools/internal/broker/storage"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestSigner(t testing.TB) *Signer {
	t.Helper()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	s, err := NewSigner(key)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

// testServerDeps is ServerDeps over a fake clientset with the config's rate
// limits, no discovery and no id_token signer; see signed.
func testServerDeps(t testing.TB, cfg *core.Config, verifier core.Verifier, k8s *fake.Clientset) ServerDeps {
	t.Helper()
	deps := ServerDeps{SharedDeps: core.SharedDeps{Verifier: verifier}, Signer: newTestSigner(t), Store: storage.NewK8sSecretStore(k8s)}
	deps.SubjectLimiter, deps.LoginLimiter = core.NewRateLimits(&cfg.RateLimit)
	return deps
}

// signed adds an id_token signer and composes the verifier, as Run does, so
// the refresh grant, JWKS and the broker signed bearer leg are live.
func (d ServerDeps) signed(cfg *core.Config, signer *core.IDTokenSigner) ServerDeps {
	d.IDTokenSigner = signer
	d.Verifier = core.VerifierWith(d.Verifier, signer, cfg.Server.PublicURL)
	return d
}

// newTestServer serves the management API over TLS, since the state cookie
// is Secure and scoped to /auth/callback. The clock stays at time.Now: a
// pinned date would expire the OIDC state cookie under any later wall clock.
func newTestServer(t *testing.T, cfg *core.Config, verifier core.Verifier, k8s *fake.Clientset) (testSrv *httptest.Server, brokerSrv *Server) {
	t.Helper()
	return newTestServerWithSigner(t, cfg, verifier, k8s, nil)
}

// newTestServerWithSigner is newTestServer with an id_token signer.
func newTestServerWithSigner(t *testing.T, cfg *core.Config, verifier core.Verifier, k8s *fake.Clientset, signer *core.IDTokenSigner) (testSrv *httptest.Server, brokerSrv *Server) {
	t.Helper()
	brokerSrv = NewServer(cfg, testServerDeps(t, cfg, verifier, k8s).signed(cfg, signer))
	testSrv = httptest.NewTLSServer(brokerSrv.Routes())
	t.Cleanup(testSrv.Close)
	return testSrv, brokerSrv
}

// testClient is a per-test copy of the server's client with the timeout
// applied; httptest returns one shared instance, so Jar and CheckRedirect
// changes would otherwise leak between tests.
func testClient(srv *httptest.Server) *http.Client {
	base := srv.Client()
	c := *base
	c.Timeout = brokertest.HTTPTimeout
	return &c
}
