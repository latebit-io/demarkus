package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/protocol"
)

// hubBrandingConfig adds a local hub world "root" beside team-a.
func hubBrandingConfig() *core.Config {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, core.WorldConfig{Name: "root", Profile: core.ProfileKnowledge, Namespace: "root", Local: true})
	cfg.Federation.Hub = "root"
	return cfg
}

type publicGet struct {
	code   int
	header http.Header
	body   string
}

func getPublic(t *testing.T, url string) publicGet {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // test server URL
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return publicGet{code: resp.StatusCode, header: resp.Header, body: string(body)}
}

// The hub's branding documents are served raw with no bearer; everything
// else under the route stays 404 (demarkus-library #90).
func TestHubBrandingIsPublicForTheHubOnly(t *testing.T) {
	const sheet = "# Stylesheet\n\n```css\n:root { --paper: #fff; }\n```\n"
	d := &fakeDispatcher{Published: map[string]fetch.Result{
		"root/.well-known/library/site.css.md":   {Response: protocol.Response{Status: protocol.StatusOK, Body: sheet}},
		"root/index.md":                          {Response: protocol.Response{Status: protocol.StatusOK, Body: "# Private"}},
		"team-a/.well-known/library/site.css.md": {Response: protocol.Response{Status: protocol.StatusOK, Body: "# Not the hub"}},
	}}
	ts := newTestMCPGatewayWith(t, hubBrandingConfig(), &brokertest.FakeVerifier{}, d)

	got := getPublic(t, ts.URL+"/worlds/root/.well-known/library/site.css.md")
	if got.code != http.StatusOK || got.body != sheet {
		t.Fatalf("hub sheet: %d %q", got.code, got.body)
	}
	if ct := got.header.Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := got.header.Get("Cache-Control"); cc == "" {
		t.Error("Cache-Control missing: the sheet is public and should cache")
	}
	for _, url := range []string{
		"/worlds/root/.well-known/library/logo.md",       // absent on the hub
		"/worlds/root/.well-known/library/index.md",      // not a branding document
		"/worlds/team-a/.well-known/library/site.css.md", // not the hub
	} {
		if got := getPublic(t, ts.URL+url); got.code != http.StatusNotFound {
			t.Errorf("%s: %d %q, want 404", url, got.code, got.body)
		}
	}
	for _, call := range d.FetchCalls {
		if call.Host != "root" {
			t.Errorf("read reached %q; only the hub may be read without a bearer", call.Host)
		}
	}
}

// Without a federation hub the route answers nothing.
func TestHubBrandingAbsentWithoutHub(t *testing.T) {
	ts := newTestMCPGatewayWith(t, mcpTestConfig(), &brokertest.FakeVerifier{}, &fakeDispatcher{})
	if got := getPublic(t, ts.URL+"/worlds/root/.well-known/library/site.css.md"); got.code != http.StatusNotFound {
		t.Errorf("no hub: %d, want 404", got.code)
	}
}

// Repeated unauthenticated reads cost the hub one fetch per document per
// TTL, absence included; a failed read is not remembered.
func TestHubBrandingCachesPerDocument(t *testing.T) {
	d := &fakeDispatcher{Published: map[string]fetch.Result{
		"root/.well-known/library/site.css.md": {Response: protocol.Response{Status: protocol.StatusOK, Body: "# v1"}},
	}}
	f := newGatewayFixture(t, hubBrandingConfig(), &brokertest.FakeVerifier{}, KnowledgeProfile())
	ts := f.serve(t, d)
	sheet, logo := ts.URL+"/worlds/root/.well-known/library/site.css.md", ts.URL+"/worlds/root/.well-known/library/logo.md"

	for range 3 {
		if got := getPublic(t, sheet); got.code != http.StatusOK || got.body != "# v1" {
			t.Fatalf("sheet: %d %q", got.code, got.body)
		}
		if got := getPublic(t, logo); got.code != http.StatusNotFound {
			t.Fatalf("logo: %d, want 404", got.code)
		}
	}
	if n := len(d.FetchCalls); n != 2 {
		t.Fatalf("hub fetched %d times within the TTL, want 2 (one per document)", n)
	}

	d.Published["root/.well-known/library/site.css.md"] = fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# v2"}}
	f.clock.Advance(45 * time.Second)
	got := getPublic(t, sheet)
	if got.body != "# v1" {
		t.Errorf("sheet changed within the TTL: %q", got.body)
	}
	// An HTTP cache may keep the entry only for what is left of its minute.
	if cc := got.header.Get("Cache-Control"); cc != "public, max-age=15" {
		t.Errorf("Cache-Control after 45s = %q, want max-age=15", cc)
	}
	f.clock.Advance(hubBrandingTTL)
	if got := getPublic(t, sheet); got.body != "# v2" {
		t.Errorf("sheet after TTL: %q, want v2", got.body)
	}

	// Published answers before FetchFn, so the hub goes down by losing the document.
	v2 := d.Published["root/.well-known/library/site.css.md"]
	delete(d.Published, "root/.well-known/library/site.css.md")
	d.FetchFn = func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
		return fetch.Result{}, errors.New("hub down")
	}
	f.clock.Advance(hubBrandingTTL)
	for range 2 {
		if got := getPublic(t, sheet); got.code != http.StatusBadGateway {
			t.Fatalf("hub down: %d, want 502", got.code)
		}
	}
	d.FetchFn, d.Published["root/.well-known/library/site.css.md"] = nil, v2
	if got := getPublic(t, sheet); got.code != http.StatusOK {
		t.Errorf("hub back: %d, want 200 (failures must not be cached)", got.code)
	}
}

// The first caller's cancellation neither fails the others sharing the
// flight nor loses the entry: the fetch runs on its own context.
func TestHubBrandingFlightOutlivesItsFirstCaller(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	d := &fakeDispatcher{FetchFn: func(ctx context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return fetch.Result{}, ctx.Err()
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# sheet"}}, nil
	}}
	g := newGatewayFixture(t, hubBrandingConfig(), &brokertest.FakeVerifier{}, KnowledgeProfile()).gateway(d)
	now := g.deps.Clock()

	first, firstCancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := g.hubBrandingEntry(first, "site.css.md", now)
		firstDone <- err
	}()
	<-started
	firstCancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller: %v, want context.Canceled", err)
	}
	close(release)
	entry, err := g.hubBrandingEntry(context.Background(), "site.css.md", now)
	if err != nil || entry.body != "# sheet" {
		t.Fatalf("after the first caller left: %+v %v", entry, err)
	}
	if n := len(d.FetchCalls); n != 1 {
		t.Errorf("hub fetched %d times, want 1: the flight should have landed", n)
	}
}
