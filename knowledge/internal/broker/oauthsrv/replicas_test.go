package oauthsrv

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"k8s.io/client-go/kubernetes/fake"
)

// An auth-code login whose three requests land on different replicas: the
// authorize on one, the IdP callback on the other, the token exchange back
// on the first. Both replicas share one Secret store, as pods in a release do.
func TestAuthCodeLoginCrossesReplicas(t *testing.T) {
	cfg := brokertest.NewConfig()
	verifier := &brokertest.FakeVerifier{
		AuthURL: "https://idp.example.com/authorize",
		Claims:  core.Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true},
	}
	k8s := fake.NewSimpleClientset()
	signer := brokertest.NewTestIDTokenSigner(t)
	first, _ := newTestServerWithSigner(t, cfg, verifier, k8s, signer)
	second, _ := newTestServerWithSigner(t, cfg, verifier, k8s, signer)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := testClient(first)
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := client.Get(first.URL + "/oauth/authorize?" + authorizeQuery().Encode())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	idp, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || idp.Query().Get("state") == "" {
		t.Fatalf("authorize on the first replica: %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// The jar keys cookies by host, not port, so the state cookie reaches
	// the second replica as it would behind one hostname.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		second.URL+"/auth/callback?code=idp-code&state="+url.QueryEscape(idp.Query().Get("state")), http.NoBody)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusFound || back.Query().Get("code") == "" {
		t.Fatalf("callback on the second replica: %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {back.Query().Get("code")},
		"client_id":     {"client-abc"},
		"redirect_uri":  {"http://127.0.0.1:55408/callback"},
		"code_verifier": {"dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
	}
	resp, err = testClient(first).Post(first.URL+"/device/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token exchange on the first replica = %d, want 200", resp.StatusCode)
	}
}
