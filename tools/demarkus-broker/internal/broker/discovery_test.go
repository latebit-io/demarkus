package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// idpDiscoveryFixture is the upstream OIDC discovery JSON the broker
// fetches. Modeled on a typical Google / Okta / Auth0 doc so the proxy
// assertions cover real-world field names; includes both the fields the
// broker overrides (issuer, device_authorization_endpoint, token_endpoint)
// and the fields it must pass through unchanged (jwks_uri,
// userinfo_endpoint, authorization_endpoint, plus IdP-specific extras
// like code_challenge_methods_supported).
const idpDiscoveryFixture = `{
  "issuer": "https://idp.example.com",
  "authorization_endpoint": "https://idp.example.com/authorize",
  "device_authorization_endpoint": "https://idp.example.com/oauth/device/code",
  "token_endpoint": "https://idp.example.com/oauth/token",
  "userinfo_endpoint": "https://idp.example.com/userinfo",
  "jwks_uri": "https://idp.example.com/.well-known/jwks.json",
  "response_types_supported": ["code"],
  "grant_types_supported": ["authorization_code", "urn:ietf:params:oauth:grant-type:device_code"],
  "code_challenge_methods_supported": ["S256"]
}`

// fakeDiscoveryIdP serves the discovery fixture and counts hits so tests can
// assert cache vs. refresh behavior without timing.
type fakeDiscoveryIdP struct {
	server *httptest.Server
	hits   atomic.Int64
	body   atomic.Value // string
	status atomic.Int32 // HTTP status; 0 = use 200
}

func newFakeDiscoveryIdP(t *testing.T) *fakeDiscoveryIdP {
	t.Helper()
	idp := &fakeDiscoveryIdP{}
	idp.body.Store(idpDiscoveryFixture)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		idp.hits.Add(1)
		status := int(idp.status.Load())
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, idp.body.Load().(string))
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *fakeDiscoveryIdP) issuer() string  { return idp.server.URL }
func (idp *fakeDiscoveryIdP) hitCount() int64 { return idp.hits.Load() }

// newTestDiscovery builds a Discovery against a fakeDiscoveryIdP using a clock the
// caller can advance. Returns both so callers can step time and assert
// per-tick behavior.
func newTestDiscovery(t *testing.T, idp *fakeDiscoveryIdP, ttl time.Duration) (*Discovery, *fakeClock) {
	t.Helper()
	clk := &fakeClock{now: time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)}
	d, err := NewDiscovery(context.Background(), DiscoveryConfig{
		BrokerURL: "https://broker.example.com",
		IdPIssuer: idp.issuer(),
		TTL:       ttl,
		Clock:     clk.Now,
	})
	if err != nil {
		t.Fatalf("NewDiscovery: %v", err)
	}
	return d, clk
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func TestNewDiscoveryRequiresBrokerURL(t *testing.T) {
	_, err := NewDiscovery(context.Background(), DiscoveryConfig{IdPIssuer: "https://idp"})
	if err == nil || !strings.Contains(err.Error(), "brokerURL is required") {
		t.Fatalf("err = %v, want brokerURL required", err)
	}
}

func TestNewDiscoveryRequiresIdPIssuer(t *testing.T) {
	_, err := NewDiscovery(context.Background(), DiscoveryConfig{BrokerURL: "https://broker"})
	if err == nil || !strings.Contains(err.Error(), "idpIssuer is required") {
		t.Fatalf("err = %v, want idpIssuer required", err)
	}
}

func TestNewDiscoveryFailsOnUnreachableIdP(t *testing.T) {
	// Closed-immediately httptest.Server gives us a URL that resolves
	// but refuses connections — the same observable shape as a real
	// network-down upstream.
	srv := httptest.NewServer(http.NewServeMux())
	srv.Close()
	_, err := NewDiscovery(context.Background(), DiscoveryConfig{
		BrokerURL: "https://broker.example.com",
		IdPIssuer: srv.URL,
	})
	if err == nil {
		t.Fatal("expected initial fetch to fail on unreachable IdP")
	}
	if !strings.Contains(err.Error(), "initial fetch") {
		t.Errorf("err = %v, want 'initial fetch' substring", err)
	}
}

func TestNewDiscoveryFailsOnUpstream5xx(t *testing.T) {
	idp := newFakeDiscoveryIdP(t)
	idp.status.Store(http.StatusServiceUnavailable)
	_, err := NewDiscovery(context.Background(), DiscoveryConfig{
		BrokerURL: "https://broker.example.com",
		IdPIssuer: idp.issuer(),
	})
	if err == nil || !strings.Contains(err.Error(), "upstream status 503") {
		t.Fatalf("err = %v, want upstream status 503", err)
	}
}

func TestDiscoveryOverridesBrokerEndpoints(t *testing.T) {
	idp := newFakeDiscoveryIdP(t)
	d, _ := newTestDiscovery(t, idp, time.Minute)
	doc := serveAndDecode(t, d)
	tests := []struct {
		field string
		want  string
	}{
		// issuer rewritten so the well-known doc identifies the broker
		// as the OIDC surface, not the IdP behind it.
		{"issuer", "https://broker.example.com"},
		// device_authorization_endpoint moved to the broker — clients
		// post their device-flow start here, not at the IdP.
		{"device_authorization_endpoint", "https://broker.example.com/device/authorize"},
		// token_endpoint moved to the broker — clients poll their
		// device-code exchange here, broker mediates with the IdP.
		{"token_endpoint", "https://broker.example.com/device/token"},
		// jwks_uri moved to the broker (PR4) because the broker now
		// signs id_tokens on the refresh-grant path. A strict client
		// fetching the broker's JWKS gets the broker's signing key,
		// matching the issuer the discovery doc advertises.
		{"jwks_uri", "https://broker.example.com/.well-known/jwks.json"},
		// registration_endpoint added so MCP clients that require RFC
		// 7591 dynamic client registration don't reject the broker with
		// "Incompatible auth server" before reaching the device-flow
		// surface. See register.go for the rubber-stamp rationale.
		{"registration_endpoint", "https://broker.example.com/register"},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			got, _ := doc[tt.field].(string)
			if got != tt.want {
				t.Errorf("%s = %q, want %q", tt.field, got, tt.want)
			}
		})
	}
}

func TestDiscoveryProxiesUnOverriddenFields(t *testing.T) {
	// Round-trips fields the broker does NOT take over:
	// userinfo_endpoint, authorization_endpoint, plus IdP-specific
	// metadata arrays. Tests both presence and exact value so a
	// regression that silently drops or rewrites these surfaces here.
	// jwks_uri WAS in this list pre-PR4 but moved to the override
	// set when the broker started signing id_tokens.
	idp := newFakeDiscoveryIdP(t)
	d, _ := newTestDiscovery(t, idp, time.Minute)
	doc := serveAndDecode(t, d)
	tests := []struct {
		field string
		want  any
	}{
		{"userinfo_endpoint", "https://idp.example.com/userinfo"},
		{"authorization_endpoint", "https://idp.example.com/authorize"},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			got := doc[tt.field]
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("%s = %v, want %v", tt.field, got, tt.want)
			}
		})
	}
	// Arrays should come through structurally intact.
	if got, ok := doc["response_types_supported"].([]any); !ok || len(got) != 1 || got[0] != "code" {
		t.Errorf("response_types_supported = %v, want [code]", doc["response_types_supported"])
	}
	if got, ok := doc["code_challenge_methods_supported"].([]any); !ok || len(got) != 1 || got[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v, want [S256]", doc["code_challenge_methods_supported"])
	}
}

func TestDiscoveryServesContentTypeAndCacheControl(t *testing.T) {
	idp := newFakeDiscoveryIdP(t)
	d, _ := newTestDiscovery(t, idp, time.Minute)
	rec := httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", http.NoBody))
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Errorf("Cache-Control = %q, want 'public, max-age=300'", got)
	}
}

func TestDiscoveryCachesWithinTTL(t *testing.T) {
	// Initial NewDiscovery counts as the first hit. Subsequent requests
	// inside the TTL window must not redrive the upstream — otherwise
	// the cache is doing nothing and a polling device-flow client would
	// fan its load straight onto the IdP.
	idp := newFakeDiscoveryIdP(t)
	d, clk := newTestDiscovery(t, idp, time.Minute)
	if got := idp.hitCount(); got != 1 {
		t.Fatalf("after construction: hits = %d, want 1", got)
	}
	for range 5 {
		clk.Advance(10 * time.Second)
		_ = serveAndDecode(t, d)
	}
	if got := idp.hitCount(); got != 1 {
		t.Errorf("after 5 calls within TTL: hits = %d, want 1", got)
	}
}

func TestDiscoveryRefreshesAfterTTL(t *testing.T) {
	idp := newFakeDiscoveryIdP(t)
	d, clk := newTestDiscovery(t, idp, time.Minute)
	clk.Advance(time.Minute + time.Second)
	_ = serveAndDecode(t, d)
	if got := idp.hitCount(); got != 2 {
		t.Errorf("after TTL elapsed: hits = %d, want 2", got)
	}
}

func TestDiscoveryServesStaleOnRefreshFailure(t *testing.T) {
	// When refresh fails (IdP suddenly returns 5xx), the handler must
	// still serve the previously-cached body — a polling client
	// benefits from a slightly-stale doc more than from a 5xx during
	// transient upstream outages.
	idp := newFakeDiscoveryIdP(t)
	d, clk := newTestDiscovery(t, idp, time.Minute)
	original := serveAndDecode(t, d)
	idp.status.Store(http.StatusServiceUnavailable)
	clk.Advance(time.Minute + time.Second)
	stale := serveAndDecode(t, d)
	if stale["issuer"] != original["issuer"] {
		t.Errorf("stale issuer = %v, want match original %v", stale["issuer"], original["issuer"])
	}
	// Confirm we actually tried to refresh (so we know we're testing
	// the stale-fallback path, not just the in-TTL cache path).
	if got := idp.hitCount(); got < 2 {
		t.Errorf("hits = %d, want >= 2 (initial + attempted refresh)", got)
	}
}

func TestDiscoveryRecoversAfterTransientFailure(t *testing.T) {
	idp := newFakeDiscoveryIdP(t)
	d, clk := newTestDiscovery(t, idp, time.Minute)
	idp.status.Store(http.StatusServiceUnavailable)
	clk.Advance(time.Minute + time.Second)
	_ = serveAndDecode(t, d) // serves stale
	idp.status.Store(0)      // back to 200
	clk.Advance(time.Minute + time.Second)
	doc := serveAndDecode(t, d)
	if doc["issuer"] != "https://broker.example.com" {
		t.Errorf("after recovery: issuer = %v, want broker URL", doc["issuer"])
	}
}

func TestDiscoveryBrokerURLTrailingSlashTrimmed(t *testing.T) {
	idp := newFakeDiscoveryIdP(t)
	d, err := NewDiscovery(context.Background(), DiscoveryConfig{
		BrokerURL: "https://broker.example.com/", // trailing slash
		IdPIssuer: idp.issuer(),
	})
	if err != nil {
		t.Fatalf("NewDiscovery: %v", err)
	}
	doc := serveAndDecode(t, d)
	// Without the trim, the override would emit
	// https://broker.example.com//device/authorize (double slash).
	if got := doc["device_authorization_endpoint"]; got != "https://broker.example.com/device/authorize" {
		t.Errorf("device_authorization_endpoint = %v, want no double slash", got)
	}
}

func TestDiscoveryRefreshSerializedAcrossConcurrentExpiry(t *testing.T) {
	// Without the refresh mutex, N concurrent goroutines arriving after
	// TTL expiry all see expired=true and each fires its own upstream
	// fetch — at scale that's a stampede on the IdP every time the
	// well-known cache rolls. The mutex serializes refreshes; late
	// arrivals re-check TTL after lock acquisition and skip the fetch
	// because a prior holder already refreshed. Assert at most ONE
	// extra upstream hit (initial + one refresh) regardless of the
	// concurrent caller count.
	idp := newFakeDiscoveryIdP(t)
	d, clk := newTestDiscovery(t, idp, time.Minute)
	if got := idp.hitCount(); got != 1 {
		t.Fatalf("after construction: hits = %d, want 1", got)
	}
	clk.Advance(time.Minute + time.Second)
	const callers = 16
	// Each worker reports its error (or nil) back through the channel.
	// We deliberately do not call t.Fatalf from inside the goroutine —
	// t.Fatalf panics the calling goroutine without releasing pending
	// sends, which would deadlock this test on the receive loop below
	// before we ever observed the failure.
	errs := make(chan error, callers)
	for range callers {
		go func() {
			_, err := serveAndDecodeErr(d)
			errs <- err
		}()
	}
	for range callers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := idp.hitCount(); got != 2 {
		t.Errorf("after %d concurrent post-TTL requests: hits = %d, want 2 (initial + 1 refresh)", callers, got)
	}
}

func TestDiscoveryRejectsInvalidUpstreamJSON(t *testing.T) {
	idp := newFakeDiscoveryIdP(t)
	idp.body.Store("not-json")
	_, err := NewDiscovery(context.Background(), DiscoveryConfig{
		BrokerURL: "https://broker.example.com",
		IdPIssuer: idp.issuer(),
	})
	if err == nil || !strings.Contains(err.Error(), "decode upstream discovery") {
		t.Fatalf("err = %v, want decode error", err)
	}
}

func serveAndDecode(t *testing.T, d *Discovery) map[string]any {
	t.Helper()
	doc, err := serveAndDecodeErr(d)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// serveAndDecodeErr is the goroutine-safe variant of serveAndDecode:
// returns errors instead of calling t.Fatalf, so tests that fan out
// across goroutines (the concurrency-stampede assertion) can surface
// failures through a channel without deadlocking the test on a panicked
// worker that never signals completion.
func serveAndDecodeErr(d *Discovery) (map[string]any, error) {
	rec := httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", http.NoBody))
	if rec.Code != http.StatusOK {
		return nil, fmt.Errorf("status = %d", rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	return doc, nil
}
