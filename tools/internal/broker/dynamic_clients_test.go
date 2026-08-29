package broker

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
