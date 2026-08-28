package snirouter

import (
	"crypto/tls"
	"errors"
	"sync/atomic"

	"github.com/latebit-io/demarkus/server/internal/quicserve"
	"github.com/quic-go/quic-go"
)

// Dynamic is an atomically swappable Router: the QUIC listener keeps one
// selector and handshake hook for life while the world set changes
// underneath. Swap validates before publishing, so routing never breaks.
type Dynamic struct {
	current atomic.Pointer[Router]
}

// NewDynamic builds a Dynamic router from the initial mappings.
func NewDynamic(mappings []Mapping) (*Dynamic, error) {
	dynamic := &Dynamic{}
	if err := dynamic.Swap(mappings); err != nil {
		return nil, err
	}
	return dynamic, nil
}

// Swap atomically replaces the mapping set. On error the previous set
// stays live.
func (d *Dynamic) Swap(mappings []Mapping) error {
	router, err := New(mappings)
	if err != nil {
		return err
	}
	d.current.Store(router)
	return nil
}

// Load returns the currently published router (nil before first Swap).
// Paired with Store for prevalidate-then-commit transitions.
func (d *Dynamic) Load() *Router { return d.current.Load() }

// Store publishes an already-validated router (from New).
func (d *Dynamic) Store(r *Router) { d.current.Store(r) }

// Selector returns a stable quicserve selector backed by the current set.
func (d *Dynamic) Selector() quicserve.Selector {
	return d.Select
}

// Select routes against the mapping set current at call time.
func (d *Dynamic) Select(connection *quic.Conn) (quicserve.Endpoint, error) {
	if connection == nil {
		return nil, errors.New("snirouter: QUIC connection is nil")
	}
	return d.lookup(connection.ConnectionState().TLS.ServerName)
}

// HandshakeHook mirrors Router.HandshakeHook against the current set.
func (d *Dynamic) HandshakeHook(config *tls.Config) (func(*tls.ClientHelloInfo) (*tls.Config, error), error) {
	return handshakeHook(config, d.lookup)
}

// lookup routes against the mapping set current at call time.
func (d *Dynamic) lookup(serverName string) (quicserve.Endpoint, error) {
	router := d.current.Load()
	if router == nil {
		return nil, errors.New("snirouter: no mappings published")
	}
	return router.lookup(serverName)
}
