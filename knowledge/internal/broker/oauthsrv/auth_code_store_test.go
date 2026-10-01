package oauthsrv

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAuthCodeIssueAndRedeem(t *testing.T) {
	ctx := context.Background()
	req := testAuthCodeRequest()
	req.Resource = "https://mcp.example.com/mcp"

	t.Run("redeems once, with the claims bound to the resource", func(t *testing.T) {
		codes := newGrantFixture(t).codes()
		code, err := codes.Issue(ctx, req, aliceClaims())
		if err != nil {
			t.Fatal(err)
		}
		claims, err := codes.Redeem(ctx, codeRedemption{Code: code, ClientID: req.ClientID, RedirectURI: req.RedirectURI, CodeVerifier: testVerifier})
		if err != nil {
			t.Fatalf("Redeem: %v", err)
		}
		if claims.Email != "alice@example.com" || claims.Resource != req.Resource {
			t.Errorf("claims = %+v, want alice bound to %s", claims, req.Resource)
		}
		if _, err := codes.Redeem(ctx, codeRedemption{Code: code, ClientID: req.ClientID, RedirectURI: req.RedirectURI, CodeVerifier: testVerifier}); !errors.Is(err, errAuthCodeNotFound) {
			t.Errorf("replay: err = %v, want errAuthCodeNotFound", err)
		}
	})

	t.Run("an unknown code is not found", func(t *testing.T) {
		codes := newGrantFixture(t).codes()
		if _, err := codes.Redeem(ctx, codeRedemption{Code: "nope", ClientID: req.ClientID, RedirectURI: req.RedirectURI, CodeVerifier: testVerifier}); !errors.Is(err, errAuthCodeNotFound) {
			t.Fatalf("err = %v, want errAuthCodeNotFound", err)
		}
	})

	t.Run("an expired code is not found", func(t *testing.T) {
		f := newGrantFixture(t)
		code, err := f.codes().Issue(ctx, req, aliceClaims())
		if err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(defaultAuthCodeTTL + time.Second)
		if _, err := f.codes().Redeem(ctx, codeRedemption{Code: code, ClientID: req.ClientID, RedirectURI: req.RedirectURI, CodeVerifier: testVerifier}); !errors.Is(err, errAuthCodeNotFound) {
			t.Fatalf("err = %v, want errAuthCodeNotFound", err)
		}
	})

	// A client's transient misconfiguration must not burn the flow.
	for _, tc := range []struct {
		name                            string
		clientID, redirectURI, verifier string
		axis                            string
	}{
		{"client_id mismatch", "other", req.RedirectURI, testVerifier, "client_id"},
		{"redirect_uri mismatch", req.ClientID, "http://127.0.0.1:1/other", testVerifier, "redirect_uri"},
		{"PKCE mismatch", req.ClientID, req.RedirectURI, "wrong-verifier", "PKCE verifier"},
	} {
		t.Run(tc.name+" keeps the code", func(t *testing.T) {
			codes := newGrantFixture(t).codes()
			code, err := codes.Issue(ctx, req, aliceClaims())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := codes.Redeem(ctx, codeRedemption{Code: code, ClientID: tc.clientID, RedirectURI: tc.redirectURI, CodeVerifier: tc.verifier}); !errors.Is(err, errAuthCodeMismatch) || !strings.HasSuffix(err.Error(), tc.axis) {
				t.Fatalf("err = %v, want a %s mismatch", err, tc.axis)
			}
			if _, err := codes.Redeem(ctx, codeRedemption{Code: code, ClientID: req.ClientID, RedirectURI: req.RedirectURI, CodeVerifier: testVerifier}); err != nil {
				t.Fatalf("the right redemption after the refusal: %v", err)
			}
		})
	}
}

// The code a callback issues on one replica redeems on another: the fix
// for logins that crossed pods.
func TestAuthCodeCrossesReplicas(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	req := testAuthCodeRequest()
	code, err := f.codes().Issue(ctx, req, aliceClaims())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.codes().Redeem(ctx, codeRedemption{Code: code, ClientID: req.ClientID, RedirectURI: req.RedirectURI, CodeVerifier: testVerifier}); err != nil {
		t.Fatalf("redeem on another replica: %v", err)
	}
}

func TestAuthCodeConcurrentRedeemOneWinner(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	req := testAuthCodeRequest()
	code, err := f.codes().Issue(ctx, req, aliceClaims())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		replica := f.codes()
		wg.Go(func() {
			_, err := replica.Redeem(ctx, codeRedemption{Code: code, ClientID: req.ClientID, RedirectURI: req.RedirectURI, CodeVerifier: testVerifier})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	var won, lost int
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, errAuthCodeNotFound):
			lost++
		default:
			t.Fatalf("unexpected err: %v", err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("won %d, lost %d; want one each", won, lost)
	}
}

func TestVerifyPKCE(t *testing.T) {
	challenge := s256(testVerifier)
	tests := []struct {
		name      string
		challenge string
		method    string
		verifier  string
		want      bool
	}{
		{"happy path S256", challenge, "S256", testVerifier, true},
		{"wrong verifier", challenge, "S256", "wrong", false},
		{"non-S256 method", challenge, "plain", testVerifier, false},
		{"empty method", challenge, "", testVerifier, false},
		{"empty challenge", "", "S256", testVerifier, false},
		{"empty verifier", challenge, "S256", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verifyPKCE(tt.challenge, tt.method, tt.verifier); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestNewAuthCodeIsBase64URL(t *testing.T) {
	code, err := newAuthCode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(code, "=+/") || len(code) != 43 {
		t.Fatalf("code %q is not 256 bits of base64url without padding", code)
	}
}
