package oauthsrv

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/storage"
	"github.com/latebit-io/demarkus/server/blob"
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

// testState is an in-memory state bucket whose operations all fail with err
// once set; see failState.
type testState struct {
	*blob.Memory
	mu  sync.Mutex
	err error
}

func newTestState(t testing.TB) *testState {
	t.Helper()
	memory, err := blob.NewMemory(64 << 10)
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	return &testState{Memory: memory}
}

func (s *testState) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *testState) fault() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *testState) Get(ctx context.Context, key string) (blob.Object, error) {
	if err := s.fault(); err != nil {
		return blob.Object{}, err
	}
	return s.Memory.Get(ctx, key)
}

func (s *testState) Head(ctx context.Context, key string) (blob.Attributes, error) {
	if err := s.fault(); err != nil {
		return blob.Attributes{}, err
	}
	return s.Memory.Head(ctx, key)
}

func (s *testState) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if err := s.fault(); err != nil {
		return blob.Attributes{}, err
	}
	return s.Memory.Create(ctx, key, data)
}

func (s *testState) Delete(ctx context.Context, key string, generation blob.Generation) error {
	if err := s.fault(); err != nil {
		return err
	}
	return s.Memory.Delete(ctx, key, generation)
}

func (s *testState) List(ctx context.Context, prefix, startAfter, cursor string) (blob.ListResult, error) {
	if err := s.fault(); err != nil {
		return blob.ListResult{}, err
	}
	return s.Memory.List(ctx, prefix, startAfter, cursor)
}

// failState makes every state bucket operation of broker fail with err.
func failState(t testing.TB, broker *Server, err error) {
	t.Helper()
	state, ok := broker.refreshStore.dir.objects.(*testState)
	if !ok {
		t.Fatalf("state bucket is %T, want *testState", broker.refreshStore.dir.objects)
	}
	state.setErr(err)
}

// testServerDeps is ServerDeps over a fake clientset and a fresh state bucket
// with the config's rate limits, no discovery and no id_token signer.
func testServerDeps(t testing.TB, cfg *core.Config, verifier core.Verifier, k8s *fake.Clientset) ServerDeps {
	t.Helper()
	deps := ServerDeps{SharedDeps: core.SharedDeps{Verifier: verifier}, Signer: newTestSigner(t), Store: storage.NewK8sSecretStore(k8s), State: newTestState(t)}
	deps.SubjectLimiter, deps.LoginLimiter = core.NewRateLimits(&cfg.RateLimit)
	return deps
}

// signed adds an id_token signer and composes the verifier, as Run does, so
// the refresh grant, JWKS and the broker signed bearer leg are live.
func (d ServerDeps) signed(cfg *core.Config, signer *core.IDTokenSigner) ServerDeps {
	d.IDTokenSigner = signer
	d.Verifier = core.VerifierWith(d.Verifier, signer, cfg.Server.PublicURL, cfg.Server.Resources())
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

// issueTestCode issues an authorization code as the callback would.
func issueTestCode(t *testing.T, broker *Server, req *AuthCodeRequest, claims *core.Claims) string {
	t.Helper()
	code, err := broker.authCodeStore.Issue(context.Background(), req, claims)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return code
}

// authorizeTestDevice starts a device grant as /device/authorize would.
func authorizeTestDevice(t *testing.T, broker *Server, resource string) (deviceCode, userCode string) {
	t.Helper()
	deviceCode, userCode, _, err := broker.deviceStore.Authorize(context.Background(), resource)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	return deviceCode, userCode
}

// bindTestDevice completes a device grant as the callback would.
func bindTestDevice(t *testing.T, broker *Server, deviceCode string, claims *core.Claims) {
	t.Helper()
	if err := broker.deviceStore.Bind(context.Background(), hashToken(deviceCode), claims); err != nil {
		t.Fatalf("Bind: %v", err)
	}
}
