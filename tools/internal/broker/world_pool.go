package broker

import (
	"context"
	"fmt"
	"sync"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// worldDispatcher is how the MCP tool handlers reach worlds; tests inject a
// fake. It takes the fetch request structs with Host carrying the WORLD NAME,
// never a dial address: *worldPool resolves the address itself.
type worldDispatcher interface {
	Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error)
	List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error)
	Versions(ctx context.Context, r fetch.VersionsRequest) (fetch.Result, error)
	Lookup(ctx context.Context, r fetch.LookupRequest) (fetch.Result, error)
	Publish(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error)
	Append(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error)
	Archive(ctx context.Context, r fetch.ArchiveRequest) (fetch.Result, error)
}

// errWorldNotFound names a world the broker has no config for; handlers map
// it to a tool error instead of a transport failure.
type errWorldNotFound struct {
	worldName string
}

func (e *errWorldNotFound) Error() string {
	return fmt.Sprintf("broker: unknown world %q (not in broker config)", e.worldName)
}

// pooledWorld caches the per-world client together with its resolved
// host so cache hits skip the config scan and address formatting.
type pooledWorld struct {
	client *fetch.Client
	host   string
}

// worldPool keeps one lazily created *fetch.Client (with its own QUIC
// connection pool) per world, resolved to a cluster-internal host:port
// via resolveWorldAddress; the mark:// scheme is implicit.
type worldPool struct {
	cfg     *Config
	mu      sync.Mutex
	clients map[string]pooledWorld
	opts    fetch.Options
}

// newWorldPool builds an empty pool; clients are created on first use. opts
// is the template every per world client inherits (TLS posture, timeouts).
func newWorldPool(cfg *Config, opts fetch.Options) *worldPool {
	return &worldPool{
		cfg:     cfg,
		clients: make(map[string]pooledWorld, len(cfg.Worlds)),
		opts:    opts,
	}
}

// resolveWorldAddress: InternalAddress wins when set, else the Kubernetes
// Service DNS name on the protocol's default port.
func resolveWorldAddress(w *WorldConfig) string {
	if w.InternalAddress != "" {
		return w.InternalAddress
	}
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", w.Name, w.Namespace, protocol.DefaultPort)
}

// Every verb is a byte for byte proxy: the world name in r.Host is swapped for
// the world's address and the world's response comes back untransformed. An
// unknown world answers errWorldNotFound.

// Fetch dispatches a FETCH; IfNoneMatch carries the graph seeder's own etag.
func (p *worldPool) Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.Fetch(ctx, r)
}

// List dispatches a LIST.
func (p *worldPool) List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.List(ctx, r)
}

// Versions dispatches a VERSIONS.
func (p *worldPool) Versions(ctx context.Context, r fetch.VersionsRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.Versions(ctx, r)
}

// Lookup dispatches a LOOKUP; the world ranks and filters.
func (p *worldPool) Lookup(ctx context.Context, r fetch.LookupRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.Lookup(ctx, r)
}

// Publish dispatches a PUBLISH; ExpectedVersion follows fetch.WriteRequest.
func (p *worldPool) Publish(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.Publish(ctx, r)
}

// Append dispatches an APPEND.
func (p *worldPool) Append(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.Append(ctx, r)
}

// Archive dispatches an ARCHIVE: history stays, status flips to archived.
func (p *worldPool) Archive(ctx context.Context, r fetch.ArchiveRequest) (fetch.Result, error) {
	c, host, err := p.clientFor(r.Host)
	if err != nil {
		return fetch.Result{}, err
	}
	r.Host = host
	return c.Archive(ctx, r)
}

// Close closes every per world client and its pooled QUIC connections; part
// of the broker's shutdown path.
func (p *worldPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, pooled := range p.clients {
		pooled.client.Close()
		delete(p.clients, name)
	}
}

// clientFor returns the worldName's lazily created client + host,
// resolving from config at call time so provisioned tenants dial without
// a pool rebuild. DialAddress dials there while SNI/URLs keep the authority.
func (p *worldPool) clientFor(worldName string) (*fetch.Client, string, error) {
	p.mu.Lock()
	if pooled, ok := p.clients[worldName]; ok {
		p.mu.Unlock()
		return pooled.client, pooled.host, nil
	}
	p.mu.Unlock()

	w, ok := p.cfg.FindWorld(worldName)
	if !ok {
		return nil, "", &errWorldNotFound{worldName: worldName}
	}
	host := resolveWorldAddress(&w)
	opts := p.opts
	if w.DialAddress != "" {
		// ServerName stays unset: fetch derives SNI from the authority
		// itself, keeping that rule in one place.
		opts.Endpoints = map[string]fetch.Endpoint{
			host: {DialAddress: w.DialAddress},
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if pooled, ok := p.clients[worldName]; ok {
		return pooled.client, pooled.host, nil
	}
	c := fetch.NewClient(opts)
	p.clients[worldName] = pooledWorld{client: c, host: host}
	return c, host, nil
}
