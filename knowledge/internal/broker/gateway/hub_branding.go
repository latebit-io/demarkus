package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"golang.org/x/sync/singleflight"
)

// hubBrandingPath serves the hub's room branding without a bearer: the library
// shows these documents on its sign-in page, so a cold library pod must read
// them before anyone signs in (ADR 0041, demarkus-library #90).
const hubBrandingPath = "/worlds/{world}/.well-known/library/{doc}"

// hubBrandingTTL bounds the hub reads an unauthenticated caller can cause to
// one per document per interval; the Cache-Control header says the same.
const hubBrandingTTL = time.Minute

// hubBrandingFetchTimeout bounds a fetch that no longer has a caller's
// context to end it.
const hubBrandingFetchTimeout = 10 * time.Second

// hubBrandingDocs is the allowlist; nothing else under the hub leaves the
// gateway unauthenticated.
var hubBrandingDocs = map[string]bool{
	"branding.md": true,
	"site.css.md": true,
	"logo.md":     true,
	"favicon.md":  true,
}

// hubBrandingEntry is one cached answer: 200 with the body, or 404.
type hubBrandingEntry struct {
	status int
	body   string
	until  time.Time
}

// hubBrandingCache remembers each document's last answer for hubBrandingTTL,
// absence included, so request volume never reaches the hub.
type hubBrandingCache struct {
	mu      sync.Mutex
	entries map[string]hubBrandingEntry
	// misses collapses concurrent reads of one document into one hub fetch.
	misses singleflight.Group
}

func (c *hubBrandingCache) get(doc string, now time.Time) (hubBrandingEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[doc]
	if !ok || !now.Before(entry.until) {
		return hubBrandingEntry{}, false
	}
	return entry, true
}

func (c *hubBrandingCache) put(doc string, entry hubBrandingEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]hubBrandingEntry)
	}
	entry.until = now.Add(hubBrandingTTL)
	c.entries[doc] = entry
}

// hubBranding answers a branding document of the federation hub as raw
// markdown, or 404 for any other world, document, or an absent document.
func (g *Gateway) hubBranding(w http.ResponseWriter, r *http.Request) {
	world, doc := r.PathValue("world"), r.PathValue("doc")
	if g.profile.TenantScoped || world != g.deps.Hub || !hubBrandingDocs[doc] || g.dispatcher == nil {
		http.NotFound(w, r)
		return
	}
	now := g.deps.Clock()
	entry, err := g.hubBrandingEntry(r.Context(), doc, now)
	if err != nil {
		g.deps.Log.WarnContext(r.Context(), "broker: hub branding read failed", "doc", doc, "err", err)
		http.Error(w, "hub unavailable", http.StatusBadGateway)
		return
	}
	if entry.status == http.StatusNotFound {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Public cache for what is left of the entry's minute, so a rebrand never
	// sits in an HTTP cache past the TTL; the library re-reads every minute.
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(max(entry.until.Sub(now), 0)/time.Second)))
	if _, err := io.WriteString(w, entry.body); err != nil {
		g.deps.Log.DebugContext(r.Context(), "broker: hub branding write failed", "doc", doc, "err", err)
	}
}

// hubBrandingEntry serves from the cache or fetches once for every caller
// waiting on the same document. A failed read is not cached, so the next
// caller retries.
func (g *Gateway) hubBrandingEntry(ctx context.Context, doc string, now time.Time) (hubBrandingEntry, error) {
	if entry, ok := g.hubBrands.get(doc, now); ok {
		return entry, nil
	}
	flight := g.hubBrands.misses.DoChan(doc, func() (any, error) {
		// A flight that just landed serves a caller that missed before it.
		if entry, ok := g.hubBrands.get(doc, g.deps.Clock()); ok {
			return entry, nil
		}
		// The fetch outlives the caller that started it: a canceled request
		// must not fail everyone sharing the flight, and the entry still lands.
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubBrandingFetchTimeout)
		defer cancel()
		entry, err := g.fetchHubBranding(fetchCtx, doc)
		if err != nil {
			return hubBrandingEntry{}, err
		}
		g.hubBrands.put(doc, entry, g.deps.Clock())
		return entry, nil
	})
	select {
	case <-ctx.Done():
		return hubBrandingEntry{}, ctx.Err()
	case fetched := <-flight:
		if fetched.Err != nil {
			return hubBrandingEntry{}, fetched.Err
		}
		return fetched.Val.(hubBrandingEntry), nil
	}
}

var errHubBrandingRefused = errors.New("hub refused the read")

// fetchHubBranding reads one document from the hub: 200 with the body, 404
// for absent or archived, an error for anything the caller should not cache.
func (g *Gateway) fetchHubBranding(ctx context.Context, doc string) (hubBrandingEntry, error) {
	docPath := path.Join("/.well-known/library", doc)
	res, err := g.dispatcher.Fetch(ctx, fetch.FetchRequest{Host: g.deps.Hub, Path: docPath})
	if err != nil {
		return hubBrandingEntry{}, fmt.Errorf("fetch %s%s: %w", g.deps.Hub, docPath, err)
	}
	switch res.Response.Status {
	case protocol.StatusOK:
		if len(res.Response.Body) > protocol.MaxBodyLength {
			return hubBrandingEntry{}, fmt.Errorf("fetch %s%s: body %d bytes over %d", g.deps.Hub, docPath, len(res.Response.Body), protocol.MaxBodyLength)
		}
		return hubBrandingEntry{status: http.StatusOK, body: res.Response.Body}, nil
	case protocol.StatusNotFound, protocol.StatusArchived:
		return hubBrandingEntry{status: http.StatusNotFound}, nil
	default:
		return hubBrandingEntry{}, fmt.Errorf("fetch %s%s: status %s: %w", g.deps.Hub, docPath, res.Response.Status, errHubBrandingRefused)
	}
}
