package broker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// TestOAuthAuthorizeAlwaysRejects covers the contract: any request shape
// — GET with query params, POST with form body, bare GET — gets the same
// 400 unsupported_response_type response. The stub is intentionally
// uniform; there is no "valid" request shape until the broker grows a
// real authorization_code grant.
func TestOAuthAuthorizeAlwaysRejects(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{
			name:   "GET with full authorize query string",
			method: http.MethodGet,
			path:   "/oauth/authorize?response_type=code&client_id=abc&redirect_uri=http%3A%2F%2Flocalhost%3A1234%2Fcallback&code_challenge=xyz&code_challenge_method=S256&state=s1",
		},
		{
			name:   "GET bare",
			method: http.MethodGet,
			path:   "/oauth/authorize",
		},
		{
			name:   "POST form-encoded",
			method: http.MethodPost,
			path:   "/oauth/authorize",
			body:   "response_type=code&client_id=abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp *http.Response
			var err error
			if tt.method == http.MethodGet {
				resp, err = testClient(srv).Get(srv.URL + tt.path)
			} else {
				resp, err = testClient(srv).Post(srv.URL+tt.path, "application/x-www-form-urlencoded", strings.NewReader(tt.body))
			}
			if err != nil {
				t.Fatalf("%s %s: %v", tt.method, tt.path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusBadRequest {
				rb, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, rb)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := resp.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want \"no-store\"", got)
			}
			var doc map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if doc["error"] != "unsupported_response_type" {
				t.Errorf("error = %q, want \"unsupported_response_type\"", doc["error"])
			}
			if !strings.Contains(doc["error_description"], "device_authorization_endpoint") {
				t.Errorf("error_description = %q, want it to mention device_authorization_endpoint", doc["error_description"])
			}
		})
	}
}

// TestOAuthAuthorizeIgnoresQueryParams confirms the stub does not echo
// caller-supplied state, redirect_uri, or client_id back in the
// response. Echoing those without validation would create XSS / open
// redirect / response-injection surface; the stub stays pure-JSON and
// declines to look at any input until a real handler replaces it.
func TestOAuthAuthorizeIgnoresQueryParams(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	q := url.Values{
		"client_id":             {"<script>alert(1)</script>"},
		"state":                 {"injected\nheader: evil"},
		"redirect_uri":          {"https://attacker.example/steal"},
		"code_challenge":        {"x"},
		"code_challenge_method": {"S256"},
	}
	resp, err := testClient(srv).Get(srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	for _, needle := range []string{"<script>", "attacker.example", "injected"} {
		if strings.Contains(string(body), needle) {
			t.Errorf("response body echoes untrusted query param %q; body=%s", needle, body)
		}
	}
	if resp.Header.Get("Location") != "" {
		t.Error("stub set Location header; should not redirect to caller-supplied redirect_uri")
	}
}
