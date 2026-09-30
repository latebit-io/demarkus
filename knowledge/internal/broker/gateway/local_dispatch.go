package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/protocol"
)

// LocalWorlds serves worlds in process, keyed by the SNI their QUIC clients
// would present; a protocol.Grant on ctx authorizes a write. Routes answers
// the startup check; Exchange on an unrouted authority errors, nothing retries.
type LocalWorlds interface {
	Routes(authority string) bool
	Exchange(ctx context.Context, authority string, req protocol.Request) (protocol.Response, error)
}

// errWorldNotLocal is a write to a world this process does not serve: the
// grant never leaves the process, so nothing could authorize it there.
var errWorldNotLocal = errors.New("not served by this broker; writes need a local world")

// Composite serves a local world in this process and every other world over
// the network, so worlds in other clusters keep working; writes are local
// only. Every verb sends the same wire request on both paths.
type Composite struct {
	worlds *core.WorldRegistry
	local  LocalWorlds
	remote WorldDispatcher
}

// NewComposite builds the dispatcher; remote is usually the *WorldPool.
func NewComposite(worlds *core.WorldRegistry, local LocalWorlds, remote WorldDispatcher) *Composite {
	return &Composite{worlds: worlds, local: local, remote: remote}
}

// CheckLocal fails when a local world is not routed by local, so a world
// the server in this process is not serving is a start error, not a slow
// path over the network to itself.
func CheckLocal(worlds *core.WorldRegistry, local LocalWorlds) error {
	all := worlds.All()
	for i := range all {
		w := &all[i]
		if !w.Local {
			continue
		}
		if local == nil {
			return fmt.Errorf("world %q is local but no knowledge server runs in this process", w.Name)
		}
		if authority := fetch.AuthorityHostname(resolveWorldAddress(w)); !local.Routes(authority) {
			return fmt.Errorf("local world %q: the knowledge server in this process does not serve %q", w.Name, authority)
		}
	}
	return nil
}

// exchange serves the encoded request locally when the world is local, else
// through remote. A local failure is the answer: the request may have run,
// so it is not retried over the network.
func (c *Composite) exchange(ctx context.Context, worldName string, encode func() (protocol.Request, error), remote func(context.Context) (fetch.Result, error)) (fetch.Result, error) {
	w, ok := c.worlds.Find(worldName)
	if !ok {
		return fetch.Result{}, &errWorldNotFound{worldName: worldName}
	}
	if !w.Local {
		return remote(ctx)
	}
	req, err := encode()
	if err != nil {
		return fetch.Result{}, err
	}
	resp, err := c.local.Exchange(ctx, fetch.AuthorityHostname(resolveWorldAddress(&w)), req)
	if err != nil {
		return fetch.Result{}, err
	}
	return fetch.Result{Response: resp}, nil
}

// write serves the encoded write under the grant on ctx. Only a local world
// can honour the grant; without one the write is refused before it runs.
func (c *Composite) write(ctx context.Context, worldName string, encode func() (protocol.Request, error)) (fetch.Result, error) {
	if _, ok := protocol.GrantFrom(ctx); !ok {
		return fetch.Result{}, core.ErrNotAuthorized
	}
	return c.exchange(ctx, worldName, encode, func(context.Context) (fetch.Result, error) {
		return fetch.Result{}, fmt.Errorf("world %q: %w", worldName, errWorldNotLocal)
	})
}

// Fetch dispatches a FETCH.
func (c *Composite) Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error) {
	return c.exchange(ctx, r.Host, r.Request, func(ctx context.Context) (fetch.Result, error) { return c.remote.Fetch(ctx, r) })
}

// List dispatches a LIST.
func (c *Composite) List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error) {
	return c.exchange(ctx, r.Host, r.Request, func(ctx context.Context) (fetch.Result, error) { return c.remote.List(ctx, r) })
}

// Versions dispatches a VERSIONS.
func (c *Composite) Versions(ctx context.Context, r fetch.VersionsRequest) (fetch.Result, error) {
	return c.exchange(ctx, r.Host, r.Request, func(ctx context.Context) (fetch.Result, error) { return c.remote.Versions(ctx, r) })
}

// Lookup dispatches a LOOKUP.
func (c *Composite) Lookup(ctx context.Context, r fetch.LookupRequest) (fetch.Result, error) {
	return c.exchange(ctx, r.Host, r.Request, func(ctx context.Context) (fetch.Result, error) { return c.remote.Lookup(ctx, r) })
}

// Publish dispatches a PUBLISH to a local world.
func (c *Composite) Publish(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	return c.write(ctx, r.Host, r.PublishRequest)
}

// Append dispatches an APPEND to a local world.
func (c *Composite) Append(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	return c.write(ctx, r.Host, r.AppendRequest)
}

// Archive dispatches an ARCHIVE to a local world.
func (c *Composite) Archive(ctx context.Context, r fetch.ArchiveRequest) (fetch.Result, error) {
	return c.write(ctx, r.Host, r.Request)
}

var _ WorldDispatcher = (*Composite)(nil)
