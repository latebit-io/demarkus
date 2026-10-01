package oauthsrv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/server/blob"
	"k8s.io/client-go/kubernetes/fake"
)

func TestTokenRevokeValid(t *testing.T) {
	srv, broker := newTestServerWithSigner(t, deviceTestConfig(), &brokertest.FakeVerifier{}, fake.NewSimpleClientset(), brokertest.NewTestIDTokenSigner(t))
	rawRefresh, err := broker.refreshStore.Issue(context.Background(),
		&core.Claims{Email: "a@b.com", EmailVerified: true}, "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	resp, err := testClient(srv).PostForm(srv.URL+"/token/revoke", url.Values{"token": {rawRefresh}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s, want 204", resp.StatusCode, body)
	}
	// Subsequent refresh of the revoked token must fail.
	if _, _, err := broker.refreshStore.Refresh(context.Background(), rawRefresh, "", admitAll); !errors.Is(err, ErrRefreshTokenInvalid) {
		t.Errorf("Refresh after Revoke err = %v, want ErrRefreshTokenInvalid", err)
	}
}

func TestTokenRevokeUnknownTokenIsNoOp(t *testing.T) {
	// RFC 7009 §2.2: revocation of an unknown / invalid token must
	// not signal back to the caller. Anything other than 204 turns
	// the endpoint into an enumeration oracle.
	srv, _ := newTestServerWithSigner(t, deviceTestConfig(), &brokertest.FakeVerifier{}, fake.NewSimpleClientset(), brokertest.NewTestIDTokenSigner(t))
	resp, err := testClient(srv).PostForm(srv.URL+"/token/revoke", url.Values{"token": {unknownRefreshToken()}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("unknown-token status = %d, want 204 (RFC 7009 §2.2)", resp.StatusCode)
	}
}

func TestTokenRevokeRejectsMissingToken(t *testing.T) {
	tests := []struct {
		name string
		form url.Values
	}{
		{"no token param", url.Values{}},
		{"empty token param", url.Values{"token": {""}}},
		{"token_type_hint without token", url.Values{"token_type_hint": {"refresh_token"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newTestServerWithSigner(t, deviceTestConfig(), &brokertest.FakeVerifier{}, fake.NewSimpleClientset(), brokertest.NewTestIDTokenSigner(t))
			resp, err := testClient(srv).PostForm(srv.URL+"/token/revoke", tt.form)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var body deviceTokenError
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Error != "invalid_request" {
				t.Errorf("error = %q, want invalid_request", body.Error)
			}
		})
	}
}

func TestTokenRevokeAcceptsTokenTypeHint(t *testing.T) {
	// token_type_hint is optional per RFC 7009 §2.1 and the broker
	// accepts it without validating. This test asserts the hint
	// does not cause rejection (regression guard against an over-
	// eager future validator).
	srv, broker := newTestServerWithSigner(t, deviceTestConfig(), &brokertest.FakeVerifier{}, fake.NewSimpleClientset(), brokertest.NewTestIDTokenSigner(t))
	rawRefresh, err := broker.refreshStore.Issue(context.Background(),
		&core.Claims{Email: "a@b.com", EmailVerified: true}, "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	resp, err := testClient(srv).PostForm(srv.URL+"/token/revoke", url.Values{
		"token":           {rawRefresh},
		"token_type_hint": {"refresh_token"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

func TestTokenRevokeIdempotent(t *testing.T) {
	srv, broker := newTestServerWithSigner(t, deviceTestConfig(), &brokertest.FakeVerifier{}, fake.NewSimpleClientset(), brokertest.NewTestIDTokenSigner(t))
	rawRefresh, err := broker.refreshStore.Issue(context.Background(),
		&core.Claims{Email: "a@b.com", EmailVerified: true}, "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	for i := range 3 {
		resp, err := testClient(srv).PostForm(srv.URL+"/token/revoke", url.Values{"token": {rawRefresh}})
		if err != nil {
			t.Fatalf("POST #%d: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("POST #%d status = %d, want 204", i, resp.StatusCode)
		}
	}
}

// A store failure on revoke is a JSON 500 (RFC 7009 §2.2.1), not the
// text/plain shape http.Error would emit.
func TestTokenRevokeServerErrorIsJSON(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	srv, broker := newTestServerWithSigner(t, deviceTestConfig(), &brokertest.FakeVerifier{}, k8s, brokertest.NewTestIDTokenSigner(t))
	rawRefresh, err := broker.refreshStore.Issue(context.Background(),
		&core.Claims{Email: "a@b.com", EmailVerified: true}, "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	failState(t, broker, blob.ErrUnavailable)

	resp, err := testClient(srv).PostForm(srv.URL+"/token/revoke", url.Values{"token": {rawRefresh}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (RFC 7009 §2.2.1)", ct)
	}
	var body deviceTokenError
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "server_error" {
		t.Errorf("error = %q, want server_error", body.Error)
	}
}
