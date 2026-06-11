package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestTokenRevokeValid(t *testing.T) {
	srv, broker := newTestServerWithSigner(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
	rawRefresh, err := broker.refreshStore.Issue(context.Background(),
		&Claims{Email: "a@b.com", EmailVerified: true}, "", time.Hour)
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
	if _, err := broker.refreshStore.Refresh(context.Background(), rawRefresh); !errors.Is(err, ErrRefreshTokenInvalid) {
		t.Errorf("Refresh after Revoke err = %v, want ErrRefreshTokenInvalid", err)
	}
}

func TestTokenRevokeUnknownTokenIsNoOp(t *testing.T) {
	// RFC 7009 §2.2: revocation of an unknown / invalid token must
	// not signal back to the caller. Anything other than 204 turns
	// the endpoint into an enumeration oracle.
	srv, _ := newTestServerWithSigner(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
	resp, err := testClient(srv).PostForm(srv.URL+"/token/revoke", url.Values{"token": {strings.Repeat("0", refreshTokenBytes*2)}})
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
			srv, _ := newTestServerWithSigner(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
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
	srv, broker := newTestServerWithSigner(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
	rawRefresh, err := broker.refreshStore.Issue(context.Background(),
		&Claims{Email: "a@b.com", EmailVerified: true}, "", time.Hour)
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
	srv, broker := newTestServerWithSigner(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
	rawRefresh, err := broker.refreshStore.Issue(context.Background(),
		&Claims{Email: "a@b.com", EmailVerified: true}, "", time.Hour)
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

// TestTokenRevokeServerErrorIsJSON pins the PR4-review fix: the
// store-failure 500 branch must serve JSON (matching RFC 7009
// §2.2.1 + the rest of the OAuth2 surface), not the text/plain
// shape `http.Error` would emit. We force an upstream k8s failure
// via a reactor so the Revoke I/O path errors out.
func TestTokenRevokeServerErrorIsJSON(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	srv, broker := newTestServerWithSigner(t, deviceTestConfig(), &fakeVerifier{}, k8s, newTestIDTokenSigner(t))
	// Mint a token so refreshStore.Revoke reaches the Update step
	// (the no-Secret-exists path returns nil and skips the failure
	// surface).
	rawRefresh, err := broker.refreshStore.Issue(context.Background(),
		&Claims{Email: "a@b.com", EmailVerified: true}, "", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Reactor that forces every secrets Get to fail. After enough
	// retries mutateSecret surrenders and propagates the error.
	k8s.PrependReactor("get", "secrets", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("simulated k8s outage")
	})

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
