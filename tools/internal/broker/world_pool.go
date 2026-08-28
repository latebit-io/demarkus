package broker

import (
	"context"
	"fmt"
	"sync"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// worldDispatcher is the read/write surface the MCP tool handlers
// use to reach worlds. The production implementation is *worldPool,
// which holds one *fetch.Client per worldName and resolves the
// internal address from WorldConfig. Tests inject a fake that
// scripts canned responses without dialing QUIC.
//
// Surface kept narrow on purpose: handlers do not see fetch.Client
// or QUIC concepts; they hand the dispatcher a worldName + path +
// token (+ body / expectedVersion / meta for the write verbs) and
// consume the resulting fetch.Result. Slice 2 added the three read
// verbs; Slice 3 adds the three write verbs. The 7 federation
// tools land in Slices 4–5.
type worldDispatcher interface {
	Fetch(worldName, path, token string) (fetch.Result, error)
	FetchContext(ctx context.Context, worldName, path, token string) (fetch.Result, error)
	FetchConditional(worldName, path, token, etag string) (fetch.Result, error)
	FetchConditionalContext(ctx context.Context, worldName, path, token, etag string) (fetch.Result, error)
	List(worldName, path, token string, opts fetch.ListOptions) (fetch.Result, error)
	Versions(worldName, path, token string) (fetch.Result, error)
	Lookup(worldName, scope, query, token string, opts fetch.LookupOptions) (fetch.Result, error)
	LookupContext(ctx context.Context, worldName, scope, query, token string, opts fetch.LookupOptions) (fetch.Result, error)
	Publish(worldName, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
	PublishContext(ctx context.Context, worldName, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
	Append(worldName, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
	Archive(worldName, path, token string) (fetch.Result, error)
}

// errWorldNotFound surfaces when a tool call targets a worldName
// that doesn't match any cfg.Worlds[]. The handler maps it to an
// MCP tool-error so the agent sees a descriptive message rather
// than a transport-level failure.
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

// newWorldPool builds a worldPool from cfg. Internal addresses are
// resolved eagerly so a typo in WorldConfig.InternalAddress fails
// at construction (visible in startup logs) rather than at first
// tool call (buried in MCP error envelopes). opts is the
// fetch.Options template every per-world client inherits; tests
// pass insecure-skip-verify, production picks up the broker's
// TLS posture from the chart.
func newWorldPool(cfg *Config, opts fetch.Options) *worldPool {
	return &worldPool{
		cfg:     cfg,
		clients: make(map[string]pooledWorld, len(cfg.Worlds)),
		opts:    opts,
	}
}

// resolveWorldAddress applies the plan v3 rule: InternalAddress
// wins when set; otherwise the standard Kubernetes Service DNS
// pattern `<name>.<namespace>.svc.cluster.local:<DefaultPort>`.
// The protocol's DefaultPort is the broker-server contract, not
// a worldPool concern, so we read it from the protocol package
// rather than hard-coding 6309.
func resolveWorldAddress(w *WorldConfig) string {
	if w.InternalAddress != "" {
		return w.InternalAddress
	}
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", w.Name, w.Namespace, protocol.DefaultPort)
}

// Fetch dispatches a FETCH against worldName. Returns
// errWorldNotFound when the broker has no WorldConfig for the
// name; otherwise forwards whatever fetch.Result the world emits
// — the broker's byte-for-byte proxy contract demands no
// transformation.
func (p *worldPool) Fetch(worldName, path, token string) (fetch.Result, error) {
	c, host, err := p.clientFor(worldName)
	if err != nil {
		return fetch.Result{}, err
	}
	return c.Fetch(host, path, token)
}

func (p *worldPool) FetchContext(ctx context.Context, worldName, path, token string) (fetch.Result, error) {
	c, host, err := p.clientFor(worldName)
	if err != nil {
		return fetch.Result{}, err
	}
	return c.FetchContext(ctx, host, path, token)
}

func (p *worldPool) FetchConditionalContext(ctx context.Context, worldName, path, token, etag string) (fetch.Result, error) {
	c, host, err := p.clientFor(worldName)
	if err != nil {
		return fetch.Result{}, err
	}
	return c.FetchConditionalContext(ctx, host, path, token, etag)
}

// FetchConditional dispatches a FETCH with an explicit if-none-match etag
// (the graph seeder tracks /graph.md freshness itself).
func (p *worldPool) FetchConditional(worldName, path, token, etag string) (fetch.Result, error) {
	c, host, err := p.clientFor(worldName)
	if err != nil {
		return fetch.Result{}, err
	}
	return c.FetchConditional(host, path, token, etag)
}

// List dispatches a LIST against worldName. Same proxy contract
// as Fetch — the world's response shape is preserved. opts forwards
// LIST options (e.g. IncludeArchived) to the world verbatim.
func (p *worldPool) List(worldName, path, token string, opts fetch.ListOptions) (fetch.Result, error) {
	c, host, err := p.clientFor(worldName)
	if err != nil {
		return fetch.Result{}, err
	}
	return c.ListWithOptions(host, path, token, opts)
}

// Versions dispatches a VERSIONS against worldName. Same proxy
// contract.
func (p *worldPool) Versions(worldName, path, token string) (fetch.Result, error) {
	c, host, err := p.clientFor(worldName)
	if err != nil {
		return fetch.Result{}, err
	}
	return c.Versions(host, path, token)
}

// Lookup dispatches a LOOKUP against worldName. Same proxy
// contract as the other reads — the world ranks and filters; the
// broker forwards the query/options and hands back the world's
// importance-ranked table verbatim.
func (p *worldPool) Lookup(worldName, scope, query, token string, opts fetch.LookupOptions) (fetch.Result, error) {
	c, host, err := p.clientFor(worldName)
	if err != nil {
		return fetch.Result{}, err
	}
	return c.Lookup(host, scope, query, token, opts)
}

func (p *worldPool) LookupContext(ctx context.Context, worldName, scope, query, token string, opts fetch.LookupOptions) (fetch.Result, error) {
	c, host, err := p.clientFor(worldName)
	if err != nil {
		return fetch.Result{}, err
	}
	return c.LookupContext(ctx, host, scope, query, token, opts)
}

// Publish dispatches a PUBLISH against worldName. Byte-for-byte
// proxy: the broker forwards body and metadata verbatim, then
// hands the world's response back without transformation.
// expectedVersion follows fetch.Client.Publish's semantics:
// <0 unconditional, 0 create-only, >0 update-only.
func (p *worldPool) Publish(worldName, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	c, host, err := p.clientFor(worldName)
	if err != nil {
		return fetch.Result{}, err
	}
	return c.Publish(host, path, body, token, expectedVersion, meta)
}

func (p *worldPool) PublishContext(ctx context.Context, worldName, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	c, host, err := p.clientFor(worldName)
	if err != nil {
		return fetch.Result{}, err
	}
	return c.PublishContext(ctx, host, path, body, token, expectedVersion, meta)
}

// Append dispatches an APPEND against worldName. The world
// enforces the expectedVersion >= 1 invariant; the dispatcher
// stays as a thin proxy so the world's protocol-level error
// envelopes (`bad-request` on missing version) flow through
// unchanged.
func (p *worldPool) Append(worldName, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	c, host, err := p.clientFor(worldName)
	if err != nil {
		return fetch.Result{}, err
	}
	return c.Append(host, path, body, token, expectedVersion, meta)
}

// Archive dispatches an ARCHIVE against worldName. The demarkus
// protocol's destructive verb is ARCHIVE (not DELETE) — the
// world keeps version history and flips status to `archived`.
func (p *worldPool) Archive(worldName, path, token string) (fetch.Result, error) {
	c, host, err := p.clientFor(worldName)
	if err != nil {
		return fetch.Result{}, err
	}
	return c.Archive(host, path, token)
}

// Close closes every per-world fetch.Client, releasing pooled
// QUIC connections. Called from main.go's shutdown path
// alongside the http.Server.Shutdown sequence so connections
// land in a clean GOAWAY rather than dangling.
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
