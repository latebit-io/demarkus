package brokertest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/tools/internal/broker/core"
)

// Shared fixtures: configs, fakes and constructors every test file builds on.

const (
	// BrokerNS is the broker's own namespace in every fixture config.
	BrokerNS = "broker-ns"
)

// HTTPTimeout fails a deadlocked handler in seconds, not at the suite timeout.
const HTTPTimeout = 5 * time.Second

// NewConfig is the base fixture: one static world, no gateway.
func NewConfig() *core.Config {
	return &core.Config{
		Server: core.ServerConfig{
			Addr:            ":0",
			CookieKey:       "dGVzdC1rZXktMTIzNDU2Nzg5MGFi",
			BrokerNamespace: BrokerNS,
			StateTTL:        5 * time.Minute,
		},
		OIDC: core.OIDCConfig{
			Issuer: "https://idp", ClientID: "c", ClientSecret: "s", RedirectURL: "r",
		},
		Worlds: []core.WorldConfig{
			{
				Name:         "team-a",
				Namespace:    "team-a",
				TokensSecret: "team-a-tokens",
				Allow:        core.AllowConfig{Domains: []string{"example.com"}},
				DefaultToken: core.TokenScope{
					Paths: []string{"/team-a/*"},
				},
			},
		},
	}
}

// NewMemoryConfig provisions two static tenants: alice owns alice-w,
// bob owns bob-w.
func NewMemoryConfig() *core.Config {
	cfg := NewConfig()
	cfg.Server.MCP = core.MCPConfig{Addr: ":0", PublicURL: "https://memory.example.com"}
	cfg.Worlds = []core.WorldConfig{
		{
			Name: "alice-w", Namespace: "alice-w", TokensSecret: "alice-w-tokens",
			Allow: core.AllowConfig{Emails: []string{"alice@example.com"}},
		},
		{
			Name: "bob-w", Namespace: "bob-w", TokensSecret: "bob-w-tokens",
			Allow: core.AllowConfig{Emails: []string{"bob@example.com"}},
		},
	}
	return cfg
}

// FakeClock is a settable clock shared by a fixture's server and gateway.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock starts a clock at now.
func NewFakeClock(now time.Time) *FakeClock {
	return &FakeClock{now: now}
}

// Now is the clock's current instant.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set moves the clock to t.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// FakeVerifier implements Verifier with predetermined claims, uncoupled
// from the OIDC library.
type FakeVerifier struct {
	AuthURL     string
	Claims      core.Claims
	RawIDToken  string
	AccessToken string
	Expiry      time.Time
	ExchErr     error
	// VerifyFn, when set, decides VerifyIDToken per token.
	VerifyFn func(raw string) (core.Claims, error)
}

// AuthCodeURL is AuthURL, or a fixed IdP URL, with the state appended.
func (f *FakeVerifier) AuthCodeURL(state string) string {
	if f.AuthURL == "" {
		return "https://idp.example.com/authorize?state=" + url.QueryEscape(state)
	}
	if strings.Contains(f.AuthURL, "?") {
		return f.AuthURL + "&state=" + url.QueryEscape(state)
	}
	return f.AuthURL + "?state=" + url.QueryEscape(state)
}

// Exchange returns the scripted result, or ExchErr.
func (f *FakeVerifier) Exchange(_ context.Context, _ string) (core.ExchangeResult, error) {
	if f.ExchErr != nil {
		return core.ExchangeResult{}, f.ExchErr
	}
	return core.ExchangeResult{
		Claims:      f.Claims,
		RawIDToken:  f.RawIDToken,
		AccessToken: f.AccessToken,
		Expiry:      f.Expiry,
	}, nil
}

// VerifyIDToken returns Claims, or what VerifyFn decides.
func (f *FakeVerifier) VerifyIDToken(_ context.Context, raw string) (core.Claims, error) {
	if f.VerifyFn != nil {
		return f.VerifyFn(raw)
	}
	return f.Claims, nil
}

// AliceClaims is the identity most gateway tests run as.
func AliceClaims() core.Claims {
	return core.Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}
}

// generateTestSigningKey is a fresh ECDSA P-256 key as PKCS#8 PEM,
// ephemeral per test.
func generateTestSigningKey(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal PKCS8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// NewTestIDTokenSigner is a broker signer over a fresh ephemeral key.
func NewTestIDTokenSigner(t *testing.T) *core.IDTokenSigner {
	t.Helper()
	s, err := core.NewIDTokenSigner(generateTestSigningKey(t))
	if err != nil {
		t.Fatalf("NewIDTokenSigner: %v", err)
	}
	return s
}

// TwoSubjectVerifier maps "alice-token" and "bob-token" to distinct
// subjects, so cross subject rate limit isolation is observable.
func TwoSubjectVerifier() *FakeVerifier {
	return &FakeVerifier{
		VerifyFn: func(raw string) (core.Claims, error) {
			switch raw {
			case "alice-token":
				return core.Claims{Email: "alice@example.com", EmailVerified: true, Subject: "google|alice"}, nil
			case "bob-token":
				return core.Claims{Email: "bob@example.com", EmailVerified: true, Subject: "google|bob"}, nil
			default:
				return core.Claims{}, errors.New("unknown bearer token")
			}
		},
	}
}

// FakeBuckets records EnsureBucket and DeleteBucket calls; Fail makes
// every EnsureBucket error.
type FakeBuckets struct {
	mu      sync.Mutex
	Created []string
	Deleted []string
	Fail    bool
}

// DeletedBuckets is a snapshot of the buckets deleted so far.
func (f *FakeBuckets) DeletedBuckets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Deleted...)
}

// EnsureBucket records bucket under Created.
func (f *FakeBuckets) EnsureBucket(_ context.Context, bucket string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Fail {
		return errors.New("bucket backend down")
	}
	f.Created = append(f.Created, bucket)
	return nil
}

// DeleteBucket records bucket under Deleted.
func (f *FakeBuckets) DeleteBucket(_ context.Context, bucket string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Deleted = append(f.Deleted, bucket)
	return nil
}

// NewProvisioningConfig is NewMemoryConfig with provisioning in mode.
func NewProvisioningConfig(mode string) *core.Config {
	cfg := NewMemoryConfig()
	cfg.Provisioning = core.ProvisioningConfig{
		Mode:            mode,
		MaxTenants:      10,
		AuthorityDomain: "memory-worlds.svc.cluster.local",
		DialAddress:     "demarkus-knowledge-server.memory-worlds.svc.cluster.local:6309",
		BucketPrefix:    "gs://memory-",
		BucketProject:   "demarkus-test",
		ServerNamespace: "memory-worlds",
		WorldsSecret:    "memory-worlds-config",
		TokensSecret:    "memory-worlds-tokens",
		TokensMountPath: "/etc/demarkus/worlds-tokens",
		RegistrySecret:  "memory-broker-registry",
	}
	return cfg
}

// EveClaims is the identity provisioning tests arrive as; the email is
// not canonical on purpose.
func EveClaims() *core.Claims {
	return &core.Claims{Subject: "google|eve-123", Email: "Eve.Adams@example.com", EmailVerified: true}
}

// MustRead reads path or fails the test.
func MustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// IdPBearer is the one IdP bearer AllowDomainsVerifier admits.
const IdPBearer = "idp-token"

// AllowDomainsVerifier admits IdPBearer as alice at hd and nothing else.
func AllowDomainsVerifier(hd string) *FakeVerifier {
	return &FakeVerifier{VerifyFn: func(raw string) (core.Claims, error) {
		if raw != IdPBearer {
			return core.Claims{}, errors.New("unexpected IdP bearer")
		}
		return core.Claims{Subject: "google|alice", Email: "alice@latebit.io", EmailVerified: true, HD: hd}, nil
	}}
}

// BrokerBearer is alice's claims at hd signed by the broker for brokerURL.
func BrokerBearer(t *testing.T, signer *core.IDTokenSigner, brokerURL, hd string) string {
	t.Helper()
	raw, err := signer.Sign(&core.Claims{Subject: "google|alice", Email: "alice@latebit.io", EmailVerified: true, HD: hd},
		brokerURL, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return raw
}

// BearerRequest is a GET / carrying raw as the bearer.
func BearerRequest(raw string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+raw)
	return req
}

// NoContent is a handler that answers 204, for middleware tests.
func NoContent() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}

// AllowDomainCase is one row of AllowDomainCases.
type AllowDomainCase struct {
	Name         string
	BrokerSigned bool
	HD           string
	WantStatus   int
}

// AllowDomainCases is the identity matrix both bearer gates must agree on
// when AllowDomains is ["latebit.io"]: direct and broker signed bearers,
// inside and outside the domain.
func AllowDomainCases() []AllowDomainCase {
	return []AllowDomainCase{
		{"accepts direct IdP bearer", false, "latebit.io", http.StatusNoContent},
		{"rejects direct IdP bearer", false, "outside.example", http.StatusUnauthorized},
		{"accepts refreshed bearer", true, "latebit.io", http.StatusNoContent},
		{"rejects refreshed bearer", true, "outside.example", http.StatusUnauthorized},
	}
}
