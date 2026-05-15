package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// defaultDiscoveryTTL is the cache lifetime of the rendered
// .well-known/openid-configuration body. Plan §Risks "discovery doc cache
// invalidation" calls for ~5 minutes: short enough that an IdP key
// rotation propagates inside a single TTL, long enough that polling
// device-flow clients (PR3) don't redrive the upstream fetch on every
// /device/token tick.
const defaultDiscoveryTTL = 5 * time.Minute

// discoveryHTTPTimeout caps each upstream IdP fetch. The broker blocks
// startup on the initial fetch via NewDiscovery, so an unresponsive IdP
// must surface as a configured-failure within seconds rather than hanging
// the pod past its readiness probe.
const discoveryHTTPTimeout = 10 * time.Second

// maxDiscoveryBodySize bounds how much we read from the upstream IdP. A
// real OIDC discovery doc is ~2-4 KiB; the limit just stops a hostile or
// misbehaving upstream from forcing the broker to buffer arbitrary bytes.
const maxDiscoveryBodySize = 1 << 20 // 1 MiB

// Discovery handles GET /.well-known/openid-configuration. It fetches the
// configured IdP's own discovery doc once at startup, overrides the
// broker-hosted endpoints (issuer + device_authorization_endpoint +
// token_endpoint), caches the rendered body, and serves it from the
// cache. After the TTL elapses the next request triggers a refresh
// inline; failed refreshes fall back to the prior cached body
// (stale-while-error) because a polling device-flow client benefits
// more from a slightly-stale doc than a 5xx.
//
// Why a broker-hosted discovery doc instead of plugin-side IdP-direct:
// the plugin sees one OIDC surface regardless of which IdP a customer
// wired up (Google, Okta, Entra, Auth0). Every per-IdP quirk — device
// endpoint paths, scope strings, error-code spellings — stays inside the
// broker, and every coding-agent plugin we eventually ship reuses the
// same client without per-IdP discovery logic. See plan §"Why broker is
// the device-flow shim, not plugin going IdP-direct".
//
// What is NOT yet end-to-end correct in PR2: jwks_uri stays pointed at
// the IdP (proxied through unchanged), but issuer is overridden to the
// broker. Until PR4 mints broker-signed id_tokens with a broker-hosted
// jwks_uri, strict OIDC-client validation against this discovery doc
// would reject id_tokens that came signed by the IdP and carry the IdP's
// own `iss` claim. The plugin's device-flow client (PR6) consumes only
// device_authorization_endpoint + token_endpoint from this doc, so the
// mismatch does not affect it; the gap closes in PR4 when broker
// re-signs.
type Discovery struct {
	brokerURL    string
	idpDiscovery string
	httpClient   *http.Client
	ttl          time.Duration
	clock        func() time.Time
	log          *slog.Logger

	mu    sync.RWMutex
	body  []byte
	until time.Time

	// refreshMu serializes TTL-expiry refreshes so N concurrent in-flight
	// requests fire at most one upstream fetch instead of N. Cheap insurance
	// — without it, a popular broker behind a fronting cache that all
	// expires the well-known doc simultaneously would multiply load on the
	// IdP by the in-flight request count. Held across the upstream fetch +
	// body publish; the regular mu still owns reads of body/until.
	refreshMu sync.Mutex
}

// DiscoveryConfig is the input bundle to NewDiscovery. Single struct so
// the constructor signature stays one-argument + error per
// /guidelines.md.
type DiscoveryConfig struct {
	// BrokerURL is the broker's externally-reachable base URL with no
	// trailing slash, e.g. https://broker.acmecorp.com. Loaded from
	// Server.PublicURL (broker config trims the slash once at load).
	BrokerURL string
	// IdPIssuer is the OIDC issuer URL, same value as OIDCConfig.Issuer.
	// NewDiscovery fetches IdPIssuer + "/.well-known/openid-configuration"
	// at startup.
	IdPIssuer string
	// HTTPClient is the client used for the upstream fetch. NewDiscovery
	// installs a 10-second timeout default when nil so a hung IdP cannot
	// pin the broker's startup past its readiness probe.
	HTTPClient *http.Client
	// TTL is the cache lifetime of the rendered doc. Defaults to
	// defaultDiscoveryTTL when zero. Negative values are accepted by the
	// constructor but render the cache effectively-disabled (every
	// request triggers a refresh) — intended only for tests.
	TTL time.Duration
	// Clock injects a controllable time source for tests. Defaults to
	// time.Now.
	Clock func() time.Time
	// Log receives stale-fallback warnings. Defaults to slog.Default()
	// when nil; production wiring passes the broker's structured logger
	// so the warnings land in the same JSON stream as everything else.
	Log *slog.Logger
}

// NewDiscovery constructs the handler and performs the initial fetch. A
// failed initial fetch is fatal — the broker would otherwise come up
// with /.well-known/openid-configuration serving 5xx, which a strict
// discovery-doc client would treat as configuration error anyway.
// Failing-fast at startup turns the misconfiguration into a pod
// CrashLoopBackOff that the operator notices immediately.
func NewDiscovery(ctx context.Context, cfg DiscoveryConfig) (*Discovery, error) {
	if cfg.BrokerURL == "" {
		return nil, fmt.Errorf("broker: discovery: brokerURL is required")
	}
	if cfg.IdPIssuer == "" {
		return nil, fmt.Errorf("broker: discovery: idpIssuer is required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: discoveryHTTPTimeout}
	}
	ttl := cfg.TTL
	if ttl == 0 {
		ttl = defaultDiscoveryTTL
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	d := &Discovery{
		brokerURL:    strings.TrimRight(cfg.BrokerURL, "/"),
		idpDiscovery: strings.TrimRight(cfg.IdPIssuer, "/") + "/.well-known/openid-configuration",
		httpClient:   client,
		ttl:          ttl,
		clock:        clock,
		log:          log,
	}
	if err := d.refresh(ctx); err != nil {
		return nil, fmt.Errorf("broker: discovery: initial fetch %s: %w", d.idpDiscovery, err)
	}
	return d, nil
}

// Handler is the http.Handler for GET /.well-known/openid-configuration.
// Public, unauthenticated, cacheable: this is the OIDC well-known surface
// every plugin client hits before the device-flow dance.
func (d *Discovery) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := d.get(r.Context())
		w.Header().Set("Content-Type", "application/json")
		// Cache-Control matches the broker-side TTL window so a CDN or
		// other intermediary sees the same caching horizon. 300s comes
		// from defaultDiscoveryTTL — kept literal here because the
		// HTTP header value is part of the wire contract and shouldn't
		// silently drift if a future patch changes the in-process TTL.
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(body)
	})
}

// get returns the cached body, refreshing inline on TTL expiry. Stale
// served on refresh failure so the well-known endpoint stays available
// during transient upstream issues. refreshMu serializes refreshes so
// concurrent expirers don't stampede the upstream IdP — late arrivals
// re-check the TTL after taking the lock and skip the fetch when a
// prior holder already refreshed.
func (d *Discovery) get(ctx context.Context) []byte {
	d.mu.RLock()
	expired := !d.clock().Before(d.until)
	d.mu.RUnlock()
	if expired {
		d.refreshMu.Lock()
		d.mu.RLock()
		stillExpired := !d.clock().Before(d.until)
		d.mu.RUnlock()
		if stillExpired {
			if err := d.refresh(ctx); err != nil {
				d.log.WarnContext(ctx, "broker: discovery refresh failed, serving stale",
					"err", err, "upstream", d.idpDiscovery)
			}
		}
		d.refreshMu.Unlock()
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.body
}

// refresh performs one upstream fetch + override pass. On success it
// publishes the new body + new TTL window under the write lock; on
// failure it leaves the prior cache untouched and returns the error
// (caller decides whether to surface or fall back to stale).
func (d *Discovery) refresh(ctx context.Context) error {
	// Belt-and-suspenders timeout on the fetch context: the default
	// http.Client has discoveryHTTPTimeout, but a caller may inject a
	// custom HTTPClient via DiscoveryConfig with no Timeout field set
	// (the constructor honors caller-supplied clients verbatim). Wrap
	// here so refresh() is bounded regardless of how the client was
	// configured and regardless of whether the caller's ctx has a
	// deadline.
	ctx, cancel := context.WithTimeout(ctx, discoveryHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.idpDiscovery, http.NoBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upstream fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBodySize))
	if err != nil {
		return fmt.Errorf("read upstream body: %w", err)
	}
	body, err := applyDiscoveryOverrides(raw, d.brokerURL)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.body = body
	d.until = d.clock().Add(d.ttl)
	d.mu.Unlock()
	return nil
}

// applyDiscoveryOverrides parses the upstream JSON, rewrites the three
// broker-hosted endpoints, and re-marshals. Decoded into map[string]any
// so unknown IdP-specific fields (Google's
// `code_challenge_methods_supported`, Auth0's `mfa_challenge_endpoint`,
// Okta's request_object_signing_alg lists, etc.) round-trip through
// without us tracking each one in a typed struct — proxying everything
// else verbatim is the point of this layer.
func applyDiscoveryOverrides(raw []byte, brokerURL string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode upstream discovery: %w", err)
	}
	doc["issuer"] = brokerURL
	doc["device_authorization_endpoint"] = brokerURL + "/device/authorize"
	doc["token_endpoint"] = brokerURL + "/device/token"
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode rendered discovery: %w", err)
	}
	return buf.Bytes(), nil
}
