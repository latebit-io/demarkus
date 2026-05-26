package broker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// postJSON is a tiny helper specific to /register: every test in this
// file POSTs a JSON body and decodes a JSON response, so wrap the
// boilerplate once instead of in each subtest. Uses testClient(srv)
// rather than http.DefaultClient because newTestServer mounts the
// broker behind an httptest TLS server with a self-signed cert.
func postJSON(t *testing.T, srv *httptest.Server, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/register", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("POST /register: %v", err)
	}
	return resp
}

func TestRegister(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	tests := []struct {
		name string
		body string
	}{
		{
			name: "full RFC 7591 metadata",
			body: `{"client_name":"Claude Code","redirect_uris":["http://localhost:1234/callback"],"grant_types":["urn:ietf:params:oauth:grant-type:device_code"]}`,
		},
		{
			name: "minimal metadata",
			body: `{"client_name":"test"}`,
		},
		{
			name: "empty object",
			body: `{}`,
		},
		{
			name: "empty body",
			body: ``,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := postJSON(t, srv, []byte(tt.body))
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusCreated {
				rb, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, rb)
			}
			var got map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			clientID, ok := got["client_id"].(string)
			if !ok || clientID == "" {
				t.Errorf("client_id missing or wrong type: %v", got["client_id"])
			}
			if _, ok := got["client_id_issued_at"].(float64); !ok {
				t.Errorf("client_id_issued_at missing or wrong type: %v", got["client_id_issued_at"])
			}
			if got["token_endpoint_auth_method"] != "none" {
				t.Errorf("token_endpoint_auth_method = %v, want \"none\"", got["token_endpoint_auth_method"])
			}
			grants, _ := got["grant_types"].([]any)
			if len(grants) != 1 || grants[0] != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Errorf("grant_types = %v, want [\"urn:ietf:params:oauth:grant-type:device_code\"]", got["grant_types"])
			}
			if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want \"no-store\"", cc)
			}
		})
	}
}

// TestRegisterEchoesRequestMetadata confirms that unknown RFC 7591 +
// RFC 7592 fields (software_id, contacts, etc.) round-trip through the
// rubber-stamp handler unchanged. Clients that submit these fields
// expect to see them echoed in the registration response per RFC
// 7591 §3.2.1 — otherwise they may treat the registration as silently
// rejected.
func TestRegisterEchoesRequestMetadata(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	body := `{
      "client_name": "Claude Code",
      "redirect_uris": ["http://localhost:1234/callback"],
      "software_id": "anthropic-cli",
      "contacts": ["alice@example.com"]
    }`
	resp := postJSON(t, srv, []byte(body))
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["client_name"] != "Claude Code" {
		t.Errorf("client_name = %v, want \"Claude Code\"", got["client_name"])
	}
	if got["software_id"] != "anthropic-cli" {
		t.Errorf("software_id = %v, want \"anthropic-cli\"", got["software_id"])
	}
	redirects, _ := got["redirect_uris"].([]any)
	if len(redirects) != 1 || redirects[0] != "http://localhost:1234/callback" {
		t.Errorf("redirect_uris = %v, want [\"http://localhost:1234/callback\"]", got["redirect_uris"])
	}
}

// TestRegisterBrokerFieldsOverrideRequest covers the case where a
// caller submits a `client_id`, `token_endpoint_auth_method`, or
// `grant_types` in the request body (by mistake or trying to claim a
// specific id / unsupported grant). The broker MUST mint its own
// client_id, pin token_endpoint_auth_method to "none", and pin
// grant_types to device_code — otherwise a caller could trick
// downstream consumers into accepting a chosen identifier or believing
// the broker supports authorization_code.
func TestRegisterBrokerFieldsOverrideRequest(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	body := `{
      "client_id": "evil-pinned-id",
      "token_endpoint_auth_method": "client_secret_basic",
      "grant_types": ["authorization_code", "refresh_token"]
    }`
	resp := postJSON(t, srv, []byte(body))
	defer func() { _ = resp.Body.Close() }()

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["client_id"] == "evil-pinned-id" {
		t.Error("broker honored caller-supplied client_id; should mint its own")
	}
	if got["token_endpoint_auth_method"] != "none" {
		t.Errorf("token_endpoint_auth_method = %v, want \"none\"", got["token_endpoint_auth_method"])
	}
	grants, _ := got["grant_types"].([]any)
	if len(grants) != 1 || grants[0] != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Errorf("grant_types = %v, want force-pinned to device_code only", got["grant_types"])
	}
}

// TestRegisterDistinctIDs confirms that two registrations against the
// same server return distinct client_ids. The broker mints fresh
// random ids per call (no persistence, no dedup), so collisions would
// indicate a broken CSPRNG path — worth catching before it reaches
// production.
func TestRegisterDistinctIDs(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	ids := make(map[string]bool)
	for range 5 {
		resp := postJSON(t, srv, []byte(`{}`))
		var got map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		_ = resp.Body.Close()
		id, _ := got["client_id"].(string)
		if id == "" {
			t.Fatal("client_id empty")
		}
		if ids[id] {
			t.Errorf("duplicate client_id minted: %s", id)
		}
		ids[id] = true
	}
}

func TestRegisterRejectsInvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	resp := postJSON(t, srv, []byte(`{not valid json`))
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got["error"] != "invalid_client_metadata" {
		t.Errorf("error = %v, want \"invalid_client_metadata\"", got["error"])
	}
}

func TestRegisterRejectsOversizedBody(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	// One byte past the limit triggers the 413 path.
	body := []byte(`{"x":"` + strings.Repeat("a", maxRegisterBodySize) + `"}`)
	resp := postJSON(t, srv, body)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestRegisterRejectsGET(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	resp, err := testClient(srv).Get(srv.URL + "/register")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}
