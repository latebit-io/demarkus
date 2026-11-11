package gateway

import (
	"context"
	"sync"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/dispatch"
)

// pooledWorld caches the per-world client together with its resolved
// host so cache hits skip the config scan and address formatting.
type pooledWorld struct {
	client *fetch.Client
	host   string
}

// WorldPool keeps one lazily created *fetch.Client (with its own QUIC
// connection pool) per world, resolved to a cluster-internal host:port
// via WorldConfig.Address; the mark:// scheme is implicit.
type WorldPool struct {
	find    func(name string) (core.WorldConfig, bool) // the registry's Find; tests interpose
	mu      sync.Mutex
	clients map[string]pooledWorld
	drops   uint64 // drop calls so far; a lookup that saw it change resolves again
	opts    fetch.Options
	// forgetDrops removes drop from the registry at Close.
	forgetDrops func()
}

// NewWorldPool builds an empty pool; clients are created on first use. opts
// is the template every per world client inherits (TLS posture, timeouts).
func NewWorldPool(worlds *core.WorldRegistry, opts fetch.Options) *WorldPool {
	p := &WorldPool{
		find:    worlds.Find,
		clients: make(map[string]pooledWorld),
		opts:    opts,
	}
	p.forgetDrops = worlds.OnDrop(p.drop)
	return p
}

// drop forgets a world that left the registry. Its client closes outside the
// lock; a request still in flight on it fails, which is right for a world
// that no longer exists.
func (p *WorldPool) drop(worldName string) {
	p.mu.Lock()
	pooled, ok := p.clients[worldName]
	delete(p.clients, worldName)
	p.drops++
	p.mu.Unlock()
	if ok {
		pooled.client.Close()
	}
}

// Every verb is a byte for byte proxy: the world name in r.Host is swapped for
// the world's address and the world's response comes back untransformed. An
// unknown world answers dispatch.WorldNotFoundError.

// Fetch dispatches a FETCH; IfNoneMatch carries the graph seeder's own etag.
func (p *WorldPool) Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.Fetch(ctx, r)
}

// List dispatches a LIST.
func (p *WorldPool) List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.List(ctx, r)
}

// Versions dispatches a VERSIONS.
func (p *WorldPool) Versions(ctx context.Context, r fetch.VersionsRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.Versions(ctx, r)
}

// Lookup dispatches a LOOKUP; the world ranks and filters.
func (p *WorldPool) Lookup(ctx context.Context, r fetch.LookupRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.Lookup(ctx, r)
}

// Publish dispatches a PUBLISH; ExpectedVersion follows fetch.WriteRequest.
func (p *WorldPool) Publish(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.Publish(ctx, r)
}

// Append dispatches an APPEND.
func (p *WorldPool) Append(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.Append(ctx, r)
}

// Archive dispatches an ARCHIVE: history stays, status flips to archived.
func (p *WorldPool) Archive(ctx context.Context, r fetch.ArchiveRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.Archive(ctx, r)
}

// Close closes every per world client and its pooled QUIC connections; part
// of the broker's shutdown path.
func (p *WorldPool) Close() {
	p.forgetDrops()
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, pooled := range p.clients {
		pooled.client.Close()
		delete(p.clients, name)
	}
}

// clientFor returns the world's lazily created client and host. A drop that
// lands between lookup and insert restarts the lookup; one after the insert
// evicts it, so no route outlives its generation and a hit skips the check.
func (p *WorldPool) clientFor(worldName string) (*fetch.Client, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if pooled, ok := p.clients[worldName]; ok {
			return pooled.client, pooled.host, nil
		}
		drops := p.drops
		p.mu.Unlock()
		w, ok := p.find(worldName)
		p.mu.Lock()
		if !ok {
			return nil, "", &dispatch.WorldNotFoundError{World: worldName}
		}
		// Another caller inserted, or a drop raced the lookup: the loop decides.
		if _, hit := p.clients[worldName]; hit || p.drops != drops {
			continue
		}
		host := w.Address()
		opts := p.opts
		if w.DialAddress != "" {
			// ServerName stays unset: fetch derives SNI from the authority
			// itself, keeping that rule in one place.
			opts.Endpoints = map[string]fetch.Endpoint{
				host: {DialAddress: w.DialAddress},
			}
		}
		c := fetch.NewClient(opts)
		p.clients[worldName] = pooledWorld{client: c, host: host}
		return c, host, nil
	}
}
