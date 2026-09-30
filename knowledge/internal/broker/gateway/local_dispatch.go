package gateway

import (
	"context"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/protocol"
)

// LocalWorlds serves worlds in process, keyed by the SNI their QUIC clients
// would present; the knowledge server is one. A world may stop routing
// between Routes and Exchange; Exchange then errors and nothing retries.
type LocalWorlds interface {
	Routes(authority string) bool
	Exchange(ctx context.Context, authority string, req protocol.Request) (protocol.Response, error)
}

// Composite dispatches to a world served in this process, else over the
// network, so worlds in other clusters keep working. Every verb sends the
// same wire request on both paths.
type Composite struct {
	worlds *core.WorldRegistry
	local  LocalWorlds
	remote WorldDispatcher
}

// NewComposite builds the dispatcher; remote is usually the *WorldPool.
func NewComposite(worlds *core.WorldRegistry, local LocalWorlds, remote WorldDispatcher) *Composite {
	return &Composite{worlds: worlds, local: local, remote: remote}
}

// exchange serves the encoded request locally when the world routes here,
// else through remote. A local failure is the answer: the request may have
// run, so it is not retried over the network.
func (c *Composite) exchange(ctx context.Context, worldName string, encode func() (protocol.Request, error), remote func(context.Context) (fetch.Result, error)) (fetch.Result, error) {
	w, ok := c.worlds.Find(worldName)
	if !ok {
		return fetch.Result{}, &errWorldNotFound{worldName: worldName}
	}
	authority := fetch.AuthorityHostname(resolveWorldAddress(&w))
	if !c.local.Routes(authority) {
		return remote(ctx)
	}
	req, err := encode()
	if err != nil {
		return fetch.Result{}, err
	}
	resp, err := c.local.Exchange(ctx, authority, req)
	if err != nil {
		return fetch.Result{}, err
	}
	return fetch.Result{Response: resp}, nil
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

// Publish dispatches a PUBLISH.
func (c *Composite) Publish(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	return c.exchange(ctx, r.Host, r.PublishRequest, func(ctx context.Context) (fetch.Result, error) { return c.remote.Publish(ctx, r) })
}

// Append dispatches an APPEND.
func (c *Composite) Append(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	return c.exchange(ctx, r.Host, r.AppendRequest, func(ctx context.Context) (fetch.Result, error) { return c.remote.Append(ctx, r) })
}

// Archive dispatches an ARCHIVE.
func (c *Composite) Archive(ctx context.Context, r fetch.ArchiveRequest) (fetch.Result, error) {
	return c.exchange(ctx, r.Host, r.Request, func(ctx context.Context) (fetch.Result, error) { return c.remote.Archive(ctx, r) })
}

var _ WorldDispatcher = (*Composite)(nil)
