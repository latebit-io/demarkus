package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// installTestConfig returns a single-world fixture with team-a's
// PublicURL set so the happy-path tests exercise the publication side
// without the PublicURL-filter excluding everything.
func installTestConfig() *Config {
	cfg := testConfig()
	cfg.Worlds[0].PublicURL = "mark://team-a.cluster.local:6309"
	return cfg
}

// installTestConfigTwoWorlds adds team-b (same domain allow, different
// PublicURL) so multi-world ordering, filtering, and partial-failure
// tests have a deterministic two-world fixture.
func installTestConfigTwoWorlds() *Config {
	cfg := installTestConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "team-b",
		Namespace:    "team-b",
		TokensSecret: "team-b-tokens",
		PublicURL:    "mark://team-b.cluster.local:6309",
		Allow:        AllowConfig{Domains: []string{"example.com"}},
		DefaultToken: TokenScope{
			Paths: []string{"/team-b/*"},
		},
	})
	return cfg
}

func aliceClaims() Claims {
	return Claims{Email: "alice@example.com", EmailVerified: true, Subject: "google|alice"}
}

// installReq constructs an authenticated GET /me/install request
// against the supplied test server. The fakeVerifier under the default
// newTestServer ignores the bearer's contents and returns its preset
// claims; any non-empty string works.
func installReq(t *testing.T, srv *httptest.Server, bearer string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/me/install", http.NoBody)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("GET /me/install: %v", err)
	}
	return resp
}

func decodeInstall(t *testing.T, resp *http.Response) installResponse {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	var got installResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body=%s: %v", body, err)
	}
	return got
}

func TestMeInstallHappyPathSingleWorld(t *testing.T) {
	cfg := installTestConfig()
	verifier := &fakeVerifier{claims: aliceClaims()}
	k8s := fake.NewSimpleClientset()
	srv, _ := newTestServer(t, cfg, verifier, k8s)

	resp := installReq(t, srv, "id-token")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	got := decodeInstall(t, resp)
	if got.Email != "alice@example.com" {
		t.Errorf("Email = %q, want alice@example.com", got.Email)
	}
	if len(got.Worlds) != 1 {
		t.Fatalf("worlds = %+v, want 1 entry", got.Worlds)
	}
	w := got.Worlds[0]
	if w.Name != "team-a" {
		t.Errorf("Name = %q, want team-a", w.Name)
	}
	if w.PublicURL != "mark://team-a.cluster.local:6309" {
		t.Errorf("PublicURL = %q, want configured URL", w.PublicURL)
	}
	// /me/install is read-only: no mint side-effect, so no
	// world-tokens Secret and no issuances Secret should appear after
	// the call. The MCP gateway provisions lazily on first dispatch.
	if _, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("team-a tokens Secret unexpectedly created by /me/install: %v", err)
	}
	if _, err := k8s.CoreV1().Secrets(testBrokerNS).Get(context.Background(), testIssuancesNS, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("issuances Secret unexpectedly created by /me/install: %v", err)
	}
}

func TestMeInstallMultiWorldOrdering(t *testing.T) {
	// cfg.Worlds is iterated in declaration order by readableWorlds,
	// and toInstallWorlds maps results in place. Pin the contract:
	// response worlds order matches cfg.Worlds.
	cfg := installTestConfigTwoWorlds()
	verifier := &fakeVerifier{claims: aliceClaims()}
	srv, _ := newTestServer(t, cfg, verifier, fake.NewSimpleClientset())

	resp := installReq(t, srv, "id-token")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := decodeInstall(t, resp)
	if len(got.Worlds) != 2 {
		t.Fatalf("worlds = %+v, want 2 entries", got.Worlds)
	}
	if got.Worlds[0].Name != "team-a" || got.Worlds[1].Name != "team-b" {
		t.Errorf("ordering = [%s,%s], want [team-a,team-b] (cfg.Worlds declaration order)",
			got.Worlds[0].Name, got.Worlds[1].Name)
	}
	if got.Worlds[1].PublicURL != "mark://team-b.cluster.local:6309" {
		t.Errorf("team-b PublicURL = %q, want configured URL", got.Worlds[1].PublicURL)
	}
}

func TestMeInstallFiltersWorldsWithoutPublicURL(t *testing.T) {
	// Two-world fixture: alice qualifies for both, team-b has no
	// PublicURL configured. /me/install reports only installable
	// worlds (PublicURL set) and writes no Secrets — provisioning is
	// lazy in the MCP gateway, not here.
	cfg := installTestConfigTwoWorlds()
	cfg.Worlds[1].PublicURL = "" // team-b is un-installable
	k8s := fake.NewSimpleClientset()
	verifier := &fakeVerifier{claims: aliceClaims()}
	srv, _ := newTestServer(t, cfg, verifier, k8s)

	resp := installReq(t, srv, "id-token")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := decodeInstall(t, resp)
	if len(got.Worlds) != 1 || got.Worlds[0].Name != "team-a" {
		t.Errorf("worlds = %+v, want only team-a", got.Worlds)
	}
	// Neither world's tokens Secret nor the issuances Secret should
	// have been touched: /me/install is a read-only listing.
	if _, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("team-a tokens Secret unexpectedly created: %v", err)
	}
	if _, err := k8s.CoreV1().Secrets("team-b").Get(context.Background(), "team-b-tokens", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("team-b tokens Secret unexpectedly created: %v", err)
	}
	if _, err := k8s.CoreV1().Secrets(testBrokerNS).Get(context.Background(), testIssuancesNS, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("issuances Secret unexpectedly created: %v", err)
	}
}

func TestMeInstallNoBearerReturns401(t *testing.T) {
	srv, _ := newTestServer(t, installTestConfig(), &fakeVerifier{claims: aliceClaims()}, fake.NewSimpleClientset())
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/me/install", http.NoBody)
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestMeInstallBadBearerReturns401(t *testing.T) {
	verifier := &fakeVerifier{verifyFn: func(string) (Claims, error) {
		return Claims{}, errors.New("invalid token")
	}}
	srv, _ := newTestServer(t, installTestConfig(), verifier, fake.NewSimpleClientset())

	resp := installReq(t, srv, "garbage")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestMeInstallIncludesReadableWorldForNonWriter(t *testing.T) {
	cfg := installTestConfig()
	cfg.Worlds[0].Allow = AllowConfig{Domains: []string{"otherco.test"}}
	verifier := &fakeVerifier{claims: aliceClaims()}
	srv, _ := newTestServer(t, cfg, verifier, fake.NewSimpleClientset())

	resp := installReq(t, srv, "id-token")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200. body=%s", resp.StatusCode, body)
	}
	got := decodeInstall(t, resp)
	if len(got.Worlds) != 1 || got.Worlds[0].Name != "team-a" {
		t.Errorf("Worlds = %+v, want readable team-a", got.Worlds)
	}
}

func TestMeInstallAllWorldsFilteredReturns200Empty(t *testing.T) {
	// team-a has no PublicURL, so no world is installable.
	cfg := installTestConfig()
	cfg.Worlds[0].PublicURL = ""
	verifier := &fakeVerifier{claims: aliceClaims()}
	srv, _ := newTestServer(t, cfg, verifier, fake.NewSimpleClientset())

	resp := installReq(t, srv, "id-token")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decodeInstall(t, resp)
	if len(got.Worlds) != 0 {
		t.Errorf("Worlds = %+v, want []", got.Worlds)
	}
}

func TestMeInstallUnverifiedEmailReturns403(t *testing.T) {
	// Defense-in-depth gate: the production Verifier rejects unverified
	// identities at the bearer-validation layer (requireAuth), but a
	// test double or a future Verifier impl might admit one. /me/install
	// must surface this as 403 so an unverified install bundle can never
	// land in plugin hands.
	verifier := &fakeVerifier{claims: Claims{Email: "alice@example.com", EmailVerified: false, Subject: "g|alice"}}
	srv, _ := newTestServer(t, installTestConfig(), verifier, fake.NewSimpleClientset())

	resp := installReq(t, srv, "id-token")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestMeInstallSetsNoStoreHeaders(t *testing.T) {
	// Bearer tokens in the response body must not be cached by browsers,
	// proxies, or intermediaries. The Cache-Control + Pragma directives
	// have to be set BEFORE WriteHeader; this test catches the
	// regression where they'd be set after writeJSON flushes headers
	// (silently dropped). Asserted on the success path; the empty-worlds
	// and partial-failure paths use the same writeJSON helper after the
	// same Set calls so they're transitively covered.
	cfg := installTestConfig()
	verifier := &fakeVerifier{claims: aliceClaims()}
	srv, _ := newTestServer(t, cfg, verifier, fake.NewSimpleClientset())

	resp := installReq(t, srv, "id-token")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("Pragma"); got != "no-cache" {
		t.Errorf("Pragma = %q, want no-cache", got)
	}
}

func TestMeInstallAcceptsBrokerSignedBearer(t *testing.T) {
	// Regression guard against PR4's compositeVerifier dispatch: a
	// refresh-renewed bearer signed by the broker's own key must
	// validate through requireAuth without hitting the IdP primary leg.
	// Wires the test server with an IDTokenSigner so the broker's
	// compositeVerifier is composed for real.
	cfg := installTestConfig()
	idTokenSigner := newTestIDTokenSigner(t)
	// Primary verifier blows up if invoked — proving the broker-signed
	// bearer never falls through to the IdP leg.
	primary := &fakeVerifier{verifyFn: func(string) (Claims, error) {
		return Claims{}, errors.New("primary verifier must not be invoked for broker-signed bearers")
	}}
	srv, _ := newTestServerWithSigner(t, cfg, primary, fake.NewSimpleClientset(), idTokenSigner)

	raw, err := idTokenSigner.Sign(&Claims{Subject: "u-alice", Email: "alice@example.com", EmailVerified: true}, cfg.Server.PublicURL, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	resp := installReq(t, srv, raw)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (broker-signed bearer should validate). body=%s", resp.StatusCode, body)
	}
	got := decodeInstall(t, resp)
	if len(got.Worlds) != 1 || got.Worlds[0].Name != "team-a" {
		t.Errorf("worlds = %+v, want team-a", got.Worlds)
	}
}
