package broker

import (
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
	FetchConditional(worldName, path, token, etag string) (fetch.Result, error)
	List(worldName, path, token string, opts fetch.ListOptions) (fetch.Result, error)
	Versions(worldName, path, token string) (fetch.Result, error)
	Lookup(worldName, scope, query, token string, opts fetch.LookupOptions) (fetch.Result, error)
	Publish(worldName, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
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

// worldPool keeps one *fetch.Client per world the broker is
// configured for, sharing a single client object across all
// requests against the same world. The client owns its own QUIC
// connection pool (see client/fetch.Client.Close); pooling clients
// per-world rather than per-request reuses QUIC connections across
// the busy worlds while keeping the pool's lifecycle local to the
// broker process.
//
// Internal-address resolution: worldName is mapped to a cluster-
// internal host:port using WorldConfig.InternalAddress when set,
// or the default `<name>.<namespace>.svc.cluster.local:6309`
// otherwise. The Mark Protocol scheme is implicit (always
// mark://); the dispatcher hands fetch.Client just the host:port,
// matching the rest of the codebase's host-string convention.
type worldPool struct {
	mu      sync.Mutex
	clients map[string]*fetch.Client
	hosts   map[string]string // worldName → host:port
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
	hosts := make(map[string]string, len(cfg.Worlds))
	for i := range cfg.Worlds {
		w := &cfg.Worlds[i]
		hosts[w.Name] = resolveWorldAddress(w)
	}
	return &worldPool{
		clients: make(map[string]*fetch.Client, len(cfg.Worlds)),
		hosts:   hosts,
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
	for name, c := range p.clients {
		c.Close()
		delete(p.clients, name)
	}
}

// clientFor returns the worldName's client + host. Clients are
// created lazily on first use so an unreachable world (e.g. a
// dev-only world configured but never targeted) doesn't burn a
// QUIC dial at startup. Returns errWorldNotFound when the
// worldName isn't in the broker's config.
func (p *worldPool) clientFor(worldName string) (*fetch.Client, string, error) {
	host, ok := p.hosts[worldName]
	if !ok {
		return nil, "", &errWorldNotFound{worldName: worldName}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[worldName]; ok {
		return c, host, nil
	}
	c := fetch.NewClient(p.opts)
	p.clients[worldName] = c
	return c, host, nil
}
