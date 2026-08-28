// Package snirouter routes QUIC connections by strict TLS server name.
package snirouter

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/latebit-io/demarkus/server/internal/quicserve"
	"github.com/quic-go/quic-go"
)

var (
	// ErrInvalidAuthority indicates an authority is not a canonical DNS name.
	ErrInvalidAuthority = errors.New("snirouter: invalid authority")
	// ErrUnknownAuthority indicates no endpoint is configured for an authority.
	ErrUnknownAuthority = errors.New("snirouter: unknown authority")
	// ErrDuplicateAuthority indicates two mappings normalize to the same authority.
	ErrDuplicateAuthority = errors.New("snirouter: duplicate authority")
)

// Mapping associates one DNS authority with one endpoint.
type Mapping struct {
	Authority string
	Endpoint  quicserve.Endpoint
}

// Router holds an immutable authority-to-endpoint mapping.
type Router struct {
	endpoints map[string]quicserve.Endpoint
}

// New validates and copies mappings into a Router.
func New(mappings []Mapping) (*Router, error) {
	endpoints := make(map[string]quicserve.Endpoint, len(mappings))
	for _, mapping := range mappings {
		authority, err := NormalizeAuthority(mapping.Authority)
		if err != nil {
			return nil, fmt.Errorf("mapping authority %q: %w", mapping.Authority, err)
		}
		if mapping.Endpoint == nil {
			return nil, fmt.Errorf("mapping authority %q: endpoint is nil", mapping.Authority)
		}
		if _, exists := endpoints[authority]; exists {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateAuthority, authority)
		}
		endpoints[authority] = mapping.Endpoint
	}
	return &Router{endpoints: endpoints}, nil
}

// NormalizeAuthority validates a DNS authority and folds ASCII letters to lowercase.
func NormalizeAuthority(authority string) (string, error) {
	if authority == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidAuthority)
	}
	if strings.HasSuffix(authority, ".") {
		return "", fmt.Errorf("%w: trailing dot", ErrInvalidAuthority)
	}
	if strings.Contains(authority, ":") {
		return "", fmt.Errorf("%w: port or IPv6 literal", ErrInvalidAuthority)
	}

	normalized := make([]byte, len(authority))
	for index := range len(authority) {
		character := authority[index]
		if character > 0x7f {
			return "", fmt.Errorf("%w: non-ASCII character", ErrInvalidAuthority)
		}
		if character >= 'A' && character <= 'Z' {
			character += 'a' - 'A'
		}
		normalized[index] = character
	}

	name := string(normalized)
	if len(name) > 253 {
		return "", fmt.Errorf("%w: name exceeds 253 bytes", ErrInvalidAuthority)
	}
	if net.ParseIP(name) != nil {
		return "", fmt.Errorf("%w: IP literal", ErrInvalidAuthority)
	}
	for label := range strings.SplitSeq(name, ".") {
		if err := validateLabel(label); err != nil {
			return "", err
		}
	}
	return name, nil
}

func validateLabel(label string) error {
	if label == "" {
		return fmt.Errorf("%w: empty DNS label", ErrInvalidAuthority)
	}
	if len(label) > 63 {
		return fmt.Errorf("%w: DNS label exceeds 63 bytes", ErrInvalidAuthority)
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return fmt.Errorf("%w: DNS label starts or ends with hyphen", ErrInvalidAuthority)
	}
	for index := range len(label) {
		character := label[index]
		if (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '-' {
			return fmt.Errorf("%w: invalid DNS label character", ErrInvalidAuthority)
		}
	}
	return nil
}

// HandshakeHook returns a strict SNI gate suitable for tls.Config.GetConfigForClient.
// Existing config and certificate selection run only after routing succeeds.
func (r *Router) HandshakeHook(config *tls.Config) (func(*tls.ClientHelloInfo) (*tls.Config, error), error) {
	return handshakeHook(config, r.lookup)
}

// handshakeHook is the one strict-SNI gate implementation, shared by the
// static Router and the swappable Dynamic so the rejection rule cannot
// diverge between them.
func handshakeHook(config *tls.Config, lookup func(string) (quicserve.Endpoint, error)) (func(*tls.ClientHelloInfo) (*tls.Config, error), error) {
	if config == nil {
		return nil, errors.New("snirouter: TLS config is nil")
	}
	previousHook := config.GetConfigForClient
	return func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if hello == nil {
			return nil, fmt.Errorf("TLS handshake: %w: missing client hello", ErrInvalidAuthority)
		}
		if _, err := lookup(hello.ServerName); err != nil {
			return nil, fmt.Errorf("TLS handshake SNI: %w", err)
		}
		if previousHook != nil {
			selected, err := previousHook(hello)
			if err != nil {
				return nil, err
			}
			if selected != nil {
				return selected, nil
			}
		}
		return config, nil
	}, nil
}

// Selector returns a quicserve selector that pins the routed endpoint per connection.
func (r *Router) Selector() quicserve.Selector {
	return r.Select
}

// Select defensively repeats SNI routing from negotiated connection state.
func (r *Router) Select(connection *quic.Conn) (quicserve.Endpoint, error) {
	if connection == nil {
		return nil, errors.New("snirouter: QUIC connection is nil")
	}
	return r.lookup(connection.ConnectionState().TLS.ServerName)
}

func (r *Router) lookup(serverName string) (quicserve.Endpoint, error) {
	authority, err := NormalizeAuthority(serverName)
	if err != nil {
		return nil, err
	}
	endpoint, exists := r.endpoints[authority]
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrUnknownAuthority, authority)
	}
	return endpoint, nil
}
