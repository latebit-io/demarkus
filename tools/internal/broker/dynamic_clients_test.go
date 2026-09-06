package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Register-then-authorize: the MCP-host path. A dynamically registered
// https redirect is trusted; an unregistered one stays refused.
func TestAuthorizeTrustsDynamicallyRegisteredRedirect(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{authURL: "https://idp.example.com/authorize"}, fake.NewSimpleClientset())
	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	const callback = "https://claude.ai/api/mcp/auth_callback"
	reg, err := client.Post(srv.URL+"/register", "application/json",
		strings.NewReader(`{"client_name":"Claude","redirect_uris":["`+callback+`"]}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer func() { _ = reg.Body.Close() }()
	if reg.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201", reg.StatusCode)
	}
	var regDoc struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(reg.Body).Decode(&regDoc); err != nil {
		t.Fatalf("decode register response: %v", err)
	}

	q := authorizeQuery()
	q.Set("client_id", regDoc.ClientID)
	q.Set("redirect_uri", callback)
	resp, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize with registered https redirect = %d, want 302 to IdP", resp.StatusCode)
	}

	// Same redirect under an UNREGISTERED client_id stays refused.
	q.Set("client_id", "not-registered")
	resp2, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize unregistered: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("authorize with unregistered client = %d, want 400", resp2.StatusCode)
	}
	// A registered client presenting a DIFFERENT redirect is refused.
	q.Set("client_id", regDoc.ClientID)
	q.Set("redirect_uri", "https://attacker.example/steal")
	resp3, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize wrong redirect: %v", err)
	}
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Errorf("authorize with unregistered redirect = %d, want 400", resp3.StatusCode)
	}
}

func TestRegisterRejectsInvalidRedirectURIs(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
	client := testClient(srv)
	for _, body := range []string{
		`{"redirect_uris":[]}`,
		`{"redirect_uris":["http://attacker.example/cb"]}`,
		`{"redirect_uris":["https://user:pw@host.example/cb"]}`,
		`{"redirect_uris":["https://host.example/cb#frag"]}`,
		`{"redirect_uris":[42]}`,
		`{"redirect_uris":["cursor://evil.example/oauth/callback"]}`,
		`{"redirect_uris":["windsurf://anysphere.cursor-mcp/oauth/callback"]}`,
		`{"redirect_uris":["https://host.example/cb","cursor://evil.example/oauth/callback"]}`,
	} {
		resp, err := client.Post(srv.URL+"/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("register %s: %v", body, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("register %s = %d, want 400", body, resp.StatusCode)
		}
	}
}

// Cursor registers a private-use scheme callback (RFC 8252 §7.1).
// The exact allowlisted URI registers and is trusted at authorize.
func TestAuthorizeTrustsAllowlistedNativeSchemeRedirect(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{authURL: "https://idp.example.com/authorize"}, fake.NewSimpleClientset())
	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	const callback = "cursor://anysphere.cursor-mcp/oauth/callback"
	reg, err := client.Post(srv.URL+"/register", "application/json",
		strings.NewReader(`{"client_name":"Cursor","redirect_uris":["`+callback+`"]}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer func() { _ = reg.Body.Close() }()
	if reg.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201", reg.StatusCode)
	}
	var regDoc struct {
		ClientID     string   `json:"client_id"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(reg.Body).Decode(&regDoc); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if len(regDoc.RedirectURIs) != 1 || regDoc.RedirectURIs[0] != callback {
		t.Fatalf("redirect_uris echoed = %v, want [%s]", regDoc.RedirectURIs, callback)
	}

	q := authorizeQuery()
	q.Set("client_id", regDoc.ClientID)
	q.Set("redirect_uri", callback)
	resp, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize with allowlisted native redirect = %d, want 302 to IdP", resp.StatusCode)
	}
}

// Per-registration shape bounds keep the count and byte caps honest.
func TestRegisterRejectsOversizedMetadata(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
	client := testClient(srv)

	manyURIs := make([]string, maxRedirectURIsPerClient+1)
	for i := range manyURIs {
		manyURIs[i] = fmt.Sprintf(`"https://host.example/cb%d"`, i)
	}
	longURI := "https://host.example/" + strings.Repeat("a", maxRedirectURILen)
	longName := strings.Repeat("n", maxClientNameLen+1)
	for _, body := range []string{
		`{"redirect_uris":[` + strings.Join(manyURIs, ",") + `]}`,
		`{"redirect_uris":["` + longURI + `"]}`,
		`{"client_name":"` + longName + `","redirect_uris":["https://host.example/cb"]}`,
	} {
		resp, err := client.Post(srv.URL+"/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("oversized registration = %d, want 400", resp.StatusCode)
		}
	}
}

// The serialized map must stay under the byte cap: max-size hostile
// registrations trigger oldest-first eviction, never unbounded growth.
func TestDynamicClientStoreEnforcesByteCap(t *testing.T) {
	cfg := testConfig()
	cfg.Server.DynamicClientsSecret = "dyn-clients"
	clientset := fake.NewSimpleClientset()
	store := NewDynamicClientStore(cfg, NewK8sSecretStore(clientset))

	uris := make([]string, maxRedirectURIsPerClient)
	for i := range uris {
		uris[i] = "https://host.example/" + strings.Repeat("x", maxRedirectURILen-30) + fmt.Sprint(i)
	}
	// Enough max-size records to exceed the byte cap several times over.
	n := maxDynamicClientsBytes/(maxRedirectURIsPerClient*maxRedirectURILen) + 20
	for i := range n {
		if err := store.Register(context.Background(), fmt.Sprintf("client-%04d", i), uris, strings.Repeat("n", maxClientNameLen)); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}

	secret, err := clientset.CoreV1().Secrets(cfg.Server.BrokerNamespace).Get(context.Background(), "dyn-clients", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read secret: %v", err)
	}
	payload := secret.Data[DynamicClientsSecretKey]
	if len(payload) > maxDynamicClientsBytes {
		t.Errorf("serialized map = %d bytes, want <= %d", len(payload), maxDynamicClientsBytes)
	}
	// Oldest evicted, newest kept.
	_, found, err := store.Lookup(context.Background(), "client-0000")
	if err != nil {
		t.Fatalf("lookup oldest: %v", err)
	}
	if found {
		t.Error("oldest registration survived byte-cap eviction")
	}
	_, found, err = store.Lookup(context.Background(), fmt.Sprintf("client-%04d", n-1))
	if err != nil {
		t.Fatalf("lookup newest: %v", err)
	}
	if !found {
		t.Error("newest registration missing after eviction")
	}
}
