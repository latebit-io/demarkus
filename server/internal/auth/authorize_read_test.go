package auth

import (
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func TestAuthorizeRead(t *testing.T) {
	const secret = "read-secret"
	const writer = "write-secret"
	ts := NewTokenStore(map[string]Token{
		protocol.HashToken(secret): {Label: "reader", Paths: []string{"/private/**", "/team/"}, Operations: []string{"read"}},
		protocol.HashToken(writer): {Label: "writer", Paths: []string{"/**"}, Operations: []string{"publish"}},
	})
	tests := []struct {
		name  string
		token string
		path  string
		want  error
	}{
		{name: "public path needs no token", path: "/docs/a.md"},
		{name: "public path ignores a bad token", token: "nope", path: "/docs/a.md"},
		{name: "protected path without a token", path: "/private/a.md", want: ErrNoToken},
		{name: "protected path with an unknown token", token: "nope", path: "/private/a.md", want: ErrInvalidToken},
		{name: "protected path with a token lacking read", token: writer, path: "/private/a.md", want: ErrNotPermitted},
		{name: "protected path with its token", token: secret, path: "/private/a.md"},
		{name: "directory pattern covers the bare directory name", token: secret, path: "/team"},
		{name: "directory pattern protects the bare directory name", path: "/team", want: ErrNoToken},
		{name: "the agent manifest is always public", path: protocol.WellKnownManifestPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ts.AuthorizeRead(tt.token, tt.path)
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Errorf("AuthorizeRead = %v, want %v", err, tt.want)
			}
		})
	}
}

// A nil store means no auth is configured, so every read is public.
func TestAuthorizeReadNilStore(t *testing.T) {
	var ts *TokenStore
	if err := ts.AuthorizeRead("", "/anything.md"); err != nil {
		t.Errorf("AuthorizeRead on a nil store = %v, want nil", err)
	}
}

func TestDenialPredicates(t *testing.T) {
	tests := []struct {
		err             error
		unauthenticated bool
		denial          bool
	}{
		{err: ErrNoToken, unauthenticated: true, denial: true},
		{err: ErrInvalidToken, unauthenticated: true, denial: true},
		{err: ErrTokenExpired, unauthenticated: true, denial: true},
		{err: ErrNotPermitted, denial: true},
		{err: errors.New("token file unreadable")},
		{err: nil},
	}
	for _, tt := range tests {
		if got := IsUnauthenticated(tt.err); got != tt.unauthenticated {
			t.Errorf("IsUnauthenticated(%v) = %v", tt.err, got)
		}
		if got := IsDenial(tt.err); got != tt.denial {
			t.Errorf("IsDenial(%v) = %v", tt.err, got)
		}
	}
}
