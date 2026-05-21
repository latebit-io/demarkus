package broker

import (
	"fmt"
	"sync"

	"github.com/latebit/demarkus/client/fetch"
	"github.com/latebit/demarkus/protocol"
)

// worldDispatcher is the read/write surface the MCP tool handlers
// use to reach worlds. The production implementation is *worldPool,
// which holds one *fetch.Client per worldName and resolves the
// internal address from WorldConfig. Tests inject a fake that
// scripts canned responses without dialing QUIC.
//
// Surface kept narrow on purpose: handlers do not see fetch.Client
// or QUIC concepts; they hand the dispatcher a worldName + path +
// token and consume the resulting fetch.Result. Slice 3 will add
// Publish/Append/Archive methods; the read-only verbs are Slice 2.
type worldDispatcher interface {
	Fetch(worldName, path, token string) (fetch.Result, error)
	List(worldName, path, token string) (fetch.Result, error)
	Versions(worldName, path, token string) (fetch.Result, error)
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

// List dispatches a LIST against worldName. Same proxy contract
// as Fetch — the world's response shape is preserved.
func (p *worldPool) List(worldName, path, token string) (fetch.Result, error) {
	c, host, err := p.clientFor(worldName)
	if err != nil {
		return fetch.Result{}, err
	}
	return c.List(host, path, token)
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
