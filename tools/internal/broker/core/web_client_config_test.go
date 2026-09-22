package core

import (
	"strings"
	"testing"
)

func TestValidateWebClients(t *testing.T) {
	valid := func() []WebClientConfig {
		return []WebClientConfig{{
			ClientID:         testWebClientID,
			ClientSecretHash: HashClientSecret(testWebClientSecret),
			RedirectURIs:     []string{testWebRedirectURI},
		}}
	}
	tests := []struct {
		name    string
		mutate  func([]WebClientConfig) []WebClientConfig
		wantErr string
	}{
		{
			"valid entry passes",
			func(c []WebClientConfig) []WebClientConfig { return c },
			"",
		},
		{
			"empty registry passes",
			func([]WebClientConfig) []WebClientConfig { return nil },
			"",
		},
		{
			"missing clientID",
			func(c []WebClientConfig) []WebClientConfig { c[0].ClientID = ""; return c },
			"clientID is required",
		},
		{
			"duplicate clientID",
			func(c []WebClientConfig) []WebClientConfig { return append(c, c[0]) },
			"duplicate clientID",
		},
		{
			"missing secret hash",
			func(c []WebClientConfig) []WebClientConfig { c[0].ClientSecretHash = ""; return c },
			"clientSecretHash must be 64 hex chars",
		},
		{
			"plaintext secret instead of hash",
			func(c []WebClientConfig) []WebClientConfig { c[0].ClientSecretHash = testWebClientSecret; return c },
			"clientSecretHash must be 64 hex chars",
		},
		{
			"uppercase hash normalized",
			func(c []WebClientConfig) []WebClientConfig {
				c[0].ClientSecretHash = strings.ToUpper(c[0].ClientSecretHash)
				return c
			},
			"",
		},
		{
			"no redirect URIs",
			func(c []WebClientConfig) []WebClientConfig { c[0].RedirectURIs = nil; return c },
			"at least one redirectURI is required",
		},
		{
			"http redirect rejected",
			func(c []WebClientConfig) []WebClientConfig {
				c[0].RedirectURIs = []string{"http://library.example.com/cb"}
				return c
			},
			"scheme must be https",
		},
		{
			"https loopback rejected",
			func(c []WebClientConfig) []WebClientConfig {
				c[0].RedirectURIs = []string{"https://localhost:8443/cb"}
				return c
			},
			"loopback hosts",
		},
		{
			"userinfo redirect rejected",
			func(c []WebClientConfig) []WebClientConfig {
				c[0].RedirectURIs = []string{"https://user@library.example.com/cb"}
				return c
			},
			"userinfo is not allowed",
		},
		{
			"fragment redirect rejected",
			func(c []WebClientConfig) []WebClientConfig {
				c[0].RedirectURIs = []string{"https://library.example.com/cb#frag"}
				return c
			},
			"fragment is not allowed",
		},
		{
			"relative redirect rejected",
			func(c []WebClientConfig) []WebClientConfig {
				c[0].RedirectURIs = []string{"/auth/callback"}
				return c
			},
			"scheme must be https",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWebClients(tt.mutate(valid()))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateWebClientsNormalizesHash(t *testing.T) {
	clients := []WebClientConfig{{
		ClientID:         testWebClientID,
		ClientSecretHash: " " + strings.ToUpper(HashClientSecret(testWebClientSecret)) + " ",
		RedirectURIs:     []string{testWebRedirectURI},
	}}
	if err := validateWebClients(clients); err != nil {
		t.Fatalf("validateWebClients: %v", err)
	}
	if got, want := clients[0].ClientSecretHash, HashClientSecret(testWebClientSecret); got != want {
		t.Errorf("hash not normalized: got %q, want %q", got, want)
	}
}

// One registered web client, the shape oauthsrv's flow tests also use.
const (
	testWebClientSecret = "web-client-secret-0123456789abcdef"
	testWebClientID     = "library-web"
	testWebRedirectURI  = "https://library.example.com/auth/callback"
)
