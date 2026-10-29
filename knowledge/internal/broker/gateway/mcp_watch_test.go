package gateway

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/mcpwatch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/memtest"
	"github.com/latebit-io/demarkus/server/knowledgeserver/knowledgetest"
	"github.com/mark3labs/mcp-go/mcp"
)

// Tests against real knowledge servers: every write waits out the bucket
// store's commit, so they run in parallel.

const watchWorld = "team"

// latencyBudget is the realtime plan's p99 from commit to watcher.
const latencyBudget = 500 * time.Millisecond

// watchSystem is one deployment of the team world: knowledge server replicas
// over shared buckets, each behind a knowledge gateway. far is a world served
// elsewhere.
type watchSystem struct {
	t       *testing.T
	cfg     *core.Config
	buckets *knowledgetest.Buckets
	tokens  string
}

func newWatchSystem(t *testing.T) *watchSystem {
	t.Helper()
	cfg := mcpTestConfig()
	cfg.Worlds = []core.WorldConfig{
		{Name: watchWorld, Namespace: watchWorld, Profile: core.ProfileKnowledge, Local: true, WriteScope: core.WriteScope{Paths: []string{"/**"}}},
		{Name: "far", Namespace: "far", Profile: core.ProfileKnowledge},
	}
	return &watchSystem{t: t, cfg: cfg, buckets: &knowledgetest.Buckets{}}
}

// protect marks /secret/ read-protected on every replica opened after it.
func (s *watchSystem) protect() {
	s.tokens = filepath.Join(s.t.TempDir(), "tokens.toml")
	body := "[tokens.reader]\nhash = \"" + protocol.HashToken("reader-token") + "\"\npaths = [\"/secret/**\"]\noperations = [\"read\"]\n"
	if err := os.WriteFile(s.tokens, []byte(body), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// watchReplica is one knowledge server and the gateway in its process.
type watchReplica struct {
	composite *Composite
	gateway   *Gateway
	server    *httptest.Server
	url       string
}

func (s *watchSystem) replica() *watchReplica {
	s.t.Helper()
	w, _ := s.cfg.Registry().Find(watchWorld)
	server := knowledgetest.Open(s.t, s.buckets, knowledgetest.World{Name: watchWorld, Authority: w.Authority(), TokensFile: s.tokens})
	return serveWatches(s.t, s.cfg, server, KnowledgeProfile())
}

// serveWatches checks the local worlds against server, as run does, and
// serves profile's gateway with subscriptions on, over HTTP.
func serveWatches(t *testing.T, cfg *core.Config, server LocalWorlds, profile *Profile) *watchReplica {
	t.Helper()
	if err := CheckLocal(cfg.Registry(), server); err != nil {
		t.Fatal(err)
	}
	composite := NewComposite(cfg.Registry(), server, nil)
	f := gatewayFixtureFor(t, cfg, core.SharedDeps{Verifier: watchVerifier()}, profile)
	deps := DepsFor(cfg, profile, f.shared, nil)
	deps.Watches = composite
	g := New(deps, "test", composite, profile)
	g.subs.grace = 200 * time.Millisecond
	t.Cleanup(func() {
		if err := g.Shutdown(context.Background()); err != nil {
			t.Errorf("gateway shutdown: %v", err)
		}
	})
	ts := httptest.NewServer(g.Routes())
	t.Cleanup(ts.Close)
	return &watchReplica{composite: composite, gateway: g, server: ts, url: ts.URL}
}

// memoryReplica is a memory gateway over alice's and bob's tenant worlds,
// served gated as production serves them.
func memoryReplica(t *testing.T) *watchReplica {
	t.Helper()
	cfg := brokertest.NewMemoryConfig()
	worlds := make([]knowledgetest.World, 0, len(cfg.Worlds))
	for i := range cfg.Worlds {
		cfg.Worlds[i].Local = true
		worlds = append(worlds, knowledgetest.World{Name: cfg.Worlds[i].Name, Authority: cfg.Worlds[i].Authority(), Gated: true})
	}
	return serveWatches(t, cfg, knowledgetest.Open(t, &knowledgetest.Buckets{}, worlds...), MemoryProfile())
}

// briefBearer lives long enough to subscribe and receive, then lapses.
const briefBearer = 3 * time.Second

// watchVerifier maps bearers to identities with real expiries, which the
// world's watch deadline reads from the wall clock.
func watchVerifier() *brokertest.FakeVerifier {
	return &brokertest.FakeVerifier{VerifyFn: func(raw string) (core.Claims, error) {
		claims := brokertest.AliceClaims()
		switch raw {
		case "alice":
			claims.Expiry = time.Now().Add(time.Hour)
		case "alice-brief":
			claims.Expiry = time.Now().Add(briefBearer)
		case "bob":
			claims = core.Claims{Subject: "google|bob", Email: "bob@example.com", EmailVerified: true, Expiry: time.Now().Add(time.Hour)}
		case "carol":
			claims = core.Claims{Subject: "google|carol", Email: "carol@example.com", EmailVerified: true, Expiry: time.Now().Add(time.Hour)}
		case "no-expiry":
		default:
			return core.Claims{}, errors.New("unknown bearer")
		}
		return claims, nil
	}}
}

func (r *watchReplica) publish(t *testing.T, path string) {
	t.Helper()
	r.publishIn(t, watchWorld, path)
}

func (r *watchReplica) publishIn(t *testing.T, world, path string) {
	t.Helper()
	ctx := protocol.WithGrant(context.Background(), protocol.Grant{Label: "test", Paths: []string{"/**"}})
	res, err := r.composite.Publish(ctx, fetch.WriteRequest{Host: world, Path: path, Body: "# " + path + "\n", ExpectedVersion: -1})
	if err != nil || !protocol.IsWriteSuccess(res.Response.Status) {
		t.Fatalf("publish %s%s: %q %v", world, path, res.Response.Status, err)
	}
}

// headCursor is the team world's feed position on r.
func (r *watchReplica) headCursor(t *testing.T) protocol.Cursor {
	t.Helper()
	w, err := r.composite.Watch(context.Background(), fetch.WatchRequest{Host: watchWorld, Path: "/"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	return w.Cursor()
}

// listenRequest is one subscriptions/listen; Meta joins the protocol _meta.
type listenRequest struct {
	bearer string
	uris   []string
	meta   map[string]any
	// noCapabilities drops the required client capabilities, which mcp-go
	// refuses only after the admission hook ran.
	noCapabilities bool
}

const listenID = 7

// listenClient reads one listen's SSE stream as decoded JSON-RPC messages.
type listenClient struct {
	msgs   chan map[string]any
	done   chan struct{}
	cancel context.CancelFunc
	mu     sync.Mutex
	err    error
}

// listenRefusal is a listen answered with an error instead of a stream.
type listenRefusal struct {
	status  int
	message string
}

func openListen(t *testing.T, base string, lr listenRequest) (*listenClient, *listenRefusal) {
	t.Helper()
	meta := map[string]any{mcp.MetaKeyProtocolVersion: mcp.ProtocolVersion20260728}
	if !lr.noCapabilities {
		meta[mcp.MetaKeyClientCapabilities] = map[string]any{}
	}
	maps.Copy(meta, lr.meta)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": listenID, "method": string(mcp.MethodSubscriptionsListen),
		"params": map[string]any{"_meta": meta, "notifications": map[string]any{"resourceSubscriptions": lr.uris}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+lr.bearer)
	req.Header.Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion20260728)
	req.Header.Set(mcp.HeaderMethod, string(mcp.MethodSubscriptionsListen))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		raw, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
		var answer struct {
			Error struct{ Message string } `json:"error"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			t.Fatalf("listen answer %d %q: %v", resp.StatusCode, raw, err)
		}
		return nil, &listenRefusal{status: resp.StatusCode, message: answer.Error.Message}
	}
	c := &listenClient{msgs: make(chan map[string]any, 4096), done: make(chan struct{}), cancel: cancel}
	go c.read(ctx, resp.Body)
	return c, nil
}

// read decodes the stream until it ends. It may outlive the test, so it
// keeps what went wrong for next instead of reporting through t.
func (c *listenClient) read(ctx context.Context, body io.ReadCloser) {
	defer close(c.done)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			c.fail(fmt.Errorf("SSE data %q: %w", data, err))
			break
		}
		c.msgs <- msg
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		c.fail(fmt.Errorf("read stream: %w", err))
	}
	if err := body.Close(); err != nil && ctx.Err() == nil {
		c.fail(fmt.Errorf("close stream: %w", err))
	}
}

func (c *listenClient) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
	}
}

func (c *listenClient) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// next is the next message, failing after within.
func (c *listenClient) next(t *testing.T, within time.Duration) map[string]any {
	t.Helper()
	select {
	case msg := <-c.msgs:
		return msg
	case <-c.done:
		select {
		case msg := <-c.msgs:
			return msg
		default:
		}
		t.Fatalf("listen stream ended: %v", c.failure())
	case <-time.After(within):
		t.Fatalf("no message within %v", within)
	}
	return nil
}

// acknowledged fails unless the stream's first message is the acknowledgement.
func (c *listenClient) acknowledged(t *testing.T) {
	t.Helper()
	if msg := c.next(t, 5*time.Second); msg["method"] != mcp.MethodNotificationSubscriptionsAcknowledged {
		t.Fatalf("first message = %v, want the acknowledgement", msg)
	}
}

// mustListen opens a listen and fails unless it is acknowledged.
func mustListen(t *testing.T, base string, lr listenRequest) *listenClient {
	t.Helper()
	l, refused := openListen(t, base, lr)
	if refused != nil {
		t.Fatalf("listen on %v refused: %+v", lr.uris, refused)
	}
	l.acknowledged(t)
	return l
}

// expectRefused fails unless the listen is refused with status.
func expectRefused(t *testing.T, base string, lr listenRequest, status string) *listenRefusal {
	t.Helper()
	_, refused := openListen(t, base, lr)
	if refused == nil || !strings.HasPrefix(refused.message, status+":") {
		t.Fatalf("refusal = %+v, want a message starting %q", refused, status+":")
	}
	return refused
}

// update is the next resources/updated notification: its uri and _meta.
func (c *listenClient) update(t *testing.T, within time.Duration) (uri string, meta map[string]any) {
	t.Helper()
	msg := c.next(t, within)
	if msg["method"] != string(mcp.MethodNotificationResourceUpdated) {
		t.Fatalf("message = %v, want a resource update", msg)
	}
	params, _ := msg["params"].(map[string]any)
	meta, _ = params["_meta"].(map[string]any)
	uri, _ = params["uri"].(string)
	return uri, meta
}

// ended fails unless the server ends the stream within d.
func (c *listenClient) ended(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(within):
		t.Fatalf("stream still open after %v", within)
	}
}

func TestListenNotifiesAPublishWithinTheBudget(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	l := mustListen(t, r.url, listenRequest{bearer: "alice", uris: []string{"mark://team/docs/", "mark://team/docs/a.md"}})

	r.publish(t, "/docs/a.md")
	published := time.Now()
	got := map[string]map[string]any{}
	for range 2 {
		uri, meta := l.update(t, 5*time.Second)
		if uri != "mark://team/docs/a.md" {
			t.Fatalf("uri = %q, want the changed document", uri)
		}
		got[meta[mcpwatch.MetaSubscription].(string)] = meta
	}
	if elapsed := time.Since(published); elapsed > latencyBudget {
		t.Errorf("notified %v after the write returned, budget %v", elapsed, latencyBudget)
	}
	for _, sub := range []string{"mark://team/docs/", "mark://team/docs/a.md"} {
		meta := got[sub]
		if meta == nil {
			t.Fatalf("no notification for subscription %s; got %v", sub, got)
		}
		if meta[mcpwatch.MetaPath] != "/docs/a.md" || meta[mcpwatch.MetaOp] != protocol.OpPublish || meta[mcpwatch.MetaVersion] != float64(1) {
			t.Errorf("%s: event meta = %v", sub, meta)
		}
		if _, err := protocol.ParseCursor(meta[mcpwatch.MetaCursor].(string)); err != nil {
			t.Errorf("%s: cursor: %v", sub, err)
		}
		if meta[mcp.MetaKeySubscriptionID] != float64(listenID) || meta[mcpwatch.MetaHash] == nil {
			t.Errorf("%s: subscription id or hash missing: %v", sub, meta)
		}
	}
}

// A publish through the gateway's own tool, by another session, reaches
// the listen.
func TestListenSeesAPublishThroughAnotherSession(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	l := mustListen(t, r.url, listenRequest{bearer: "alice", uris: []string{"mark://team/notes/"}})
	initR := mcpRequest(t, r.url, "bob", "", initializeRequest(1))
	session := initR.Headers[mcpSessionHeader]
	call := mcpRequest(t, r.url, "bob", session, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
		"name": "mark_publish",
		"arguments": map[string]any{
			"url": "mark://team/notes/n.md", "body": "# Note\n\nA note for the watch test.\n", "expected_version": 0,
			"metadata": map[string]any{"tags": "test,watch", "importance": "0.3", "type": "Note"},
		},
	}})
	if call.Result == nil || call.Result["isError"] == true {
		t.Fatalf("mark_publish: %s", call.RawBody)
	}
	uri, meta := l.update(t, 5*time.Second)
	if uri != "mark://team/notes/n.md" || meta[mcpwatch.MetaUser] != "bob@example.com" {
		t.Errorf("update %s %v, want bob's publish of notes/n.md", uri, meta)
	}
}

func TestListenOmitsReadProtectedPaths(t *testing.T) {
	t.Parallel()
	s := newWatchSystem(t)
	s.protect()
	r := s.replica()
	l := mustListen(t, r.url, listenRequest{bearer: "alice", uris: []string{"mark://team/"}})
	r.publish(t, "/secret/plan.md")
	r.publish(t, "/open/note.md")
	if uri, _ := l.update(t, 5*time.Second); uri != "mark://team/open/note.md" {
		t.Fatalf("first update = %s, want the open note; the secret must never notify", uri)
	}
	expectRefused(t, r.url, listenRequest{bearer: "alice", uris: []string{"mark://team/secret/"}}, protocol.StatusUnauthorized)
}

// Renewal: a second listen under a fresh bearer, from the cursor the client
// consumed, opened before the first lapses, loses nothing. The lapsed one
// ends with unauthorized and its cursor, then the stream ends.
func TestListenRenewalAcrossABearerRotationLosesNothing(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	const sub = "mark://team/r/"
	old := mustListen(t, r.url, listenRequest{bearer: "alice-brief", uris: []string{sub}})
	r.publish(t, "/r/1.md")
	_, meta := old.update(t, 5*time.Second)
	consumed := meta[mcpwatch.MetaCursor].(string)

	r.publish(t, "/r/2.md")
	renewed := mustListen(t, r.url, listenRequest{bearer: "alice", uris: []string{sub}, meta: map[string]any{mcpwatch.MetaSince: map[string]string{sub: consumed}}})
	r.publish(t, "/r/3.md")
	for _, want := range []string{"mark://team/r/2.md", "mark://team/r/3.md"} {
		if uri, _ := renewed.update(t, 5*time.Second); uri != want {
			t.Fatalf("renewed update = %s, want %s", uri, want)
		}
	}

	// The old stream may still carry 2 and 3 before its terminal notice.
	last := consumed
	for {
		uri, meta := old.update(t, briefBearer+5*time.Second)
		status, ok := meta[mcpwatch.MetaStatus]
		if !ok {
			last = meta[mcpwatch.MetaCursor].(string)
			continue
		}
		if status != protocol.StatusUnauthorized || uri != sub || meta[mcpwatch.MetaCursor] != last {
			t.Fatalf("terminal %s %v, want unauthorized on %s at the last delivered cursor %s", uri, meta, sub, last)
		}
		break
	}
	old.ended(t, 5*time.Second)
}

func TestListenResumesOnAnotherReplica(t *testing.T) {
	t.Parallel()
	s := newWatchSystem(t)
	a, b := s.replica(), s.replica()
	const sub = "mark://team/x/"
	la := mustListen(t, a.url, listenRequest{bearer: "alice", uris: []string{sub}})
	a.publish(t, "/x/1.md")
	_, meta := la.update(t, 5*time.Second)
	la.cancel()

	a.publish(t, "/x/2.md")
	// Bring b to the head first: resuming is the binding's, catching up the
	// replica's, which a read does at once.
	if _, err := b.composite.Fetch(context.Background(), fetch.FetchRequest{Host: watchWorld, Path: "/x/2.md"}); err != nil {
		t.Fatal(err)
	}
	lb := mustListen(t, b.url, listenRequest{bearer: "alice", uris: []string{sub}, meta: map[string]any{mcpwatch.MetaSince: map[string]string{sub: meta[mcpwatch.MetaCursor].(string)}}})
	if uri, meta := lb.update(t, 10*time.Second); uri != "mark://team/x/2.md" || meta[mcpwatch.MetaResync] != nil {
		t.Fatalf("resumed update on b = %s %v, want x/2.md replayed", uri, meta)
	}
}

// Nothing reaches a listen before its acknowledgement, even a backlog that is
// ready the moment the watches attach.
func TestListenDeliversNothingBeforeTheAcknowledgement(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	head := r.headCursor(t)
	for i := range 3 {
		r.publish(t, "/b/"+strconv.Itoa(i)+".md")
	}
	l := mustListen(t, r.url, listenRequest{bearer: "alice", uris: []string{"mark://team/b/"}, meta: map[string]any{mcpwatch.MetaSince: map[string]string{"mark://team/b/": head.String()}}})
	for i := range 3 {
		if uri, _ := l.update(t, 5*time.Second); uri != "mark://team/b/"+strconv.Itoa(i)+".md" {
			t.Fatalf("backlog update %d = %s", i, uri)
		}
	}
}

// recordingWatcher keeps every watch it opens.
type recordingWatcher struct {
	inner   WorldWatcher
	mu      sync.Mutex
	watches []*fetch.Watch
}

func (r *recordingWatcher) Watch(ctx context.Context, req fetch.WatchRequest) (*fetch.Watch, error) {
	w, err := r.inner.Watch(ctx, req)
	if err == nil {
		r.mu.Lock()
		r.watches = append(r.watches, w)
		r.mu.Unlock()
	}
	return w, err
}

// mcp-go refuses a listen without client capabilities after the hook
// attached its watches; the request's end lets them go.
func TestRefusedListenLeavesNoSubscription(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	recorder := &recordingWatcher{inner: r.composite}
	r.gateway.subs.source = recorder
	_, refused := openListen(t, r.url, listenRequest{bearer: "alice", uris: []string{"mark://team/"}, noCapabilities: true})
	if refused == nil || refused.status != http.StatusBadRequest {
		t.Fatalf("listen without capabilities = %+v, want refused", refused)
	}
	r.waitSubscriptions(t, 0)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.watches) != 1 {
		t.Fatalf("the hook opened %d watches, want 1 before mcp-go refused", len(recorder.watches))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := recorder.watches[0].Next(ctx); errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the watch outlived the refused listen")
	}
}

func TestListenAdmission(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	r.gateway.subs.maxPerListen = 2
	tests := []struct {
		name string
		lr   listenRequest
		want string
	}{
		{name: "unknown world", lr: listenRequest{uris: []string{"mark://nope/"}}, want: protocol.StatusNotFound},
		{name: "world served elsewhere", lr: listenRequest{uris: []string{"mark://far/"}}, want: protocol.StatusNotFound},
		{name: "not a mark URI", lr: listenRequest{uris: []string{"https://team/"}}, want: protocol.StatusBadRequest},
		{name: "query", lr: listenRequest{uris: []string{"mark://team/a.md?v=1"}}, want: protocol.StatusBadRequest},
		{name: "too many URIs", lr: listenRequest{uris: []string{"mark://team/a/", "mark://team/b/", "mark://team/c/"}}, want: protocol.StatusRateLimited},
		{name: "malformed since", lr: listenRequest{uris: []string{"mark://team/"}, meta: map[string]any{mcpwatch.MetaSince: map[string]string{"mark://team/": "nope"}}}, want: protocol.StatusBadRequest},
		{name: "since for another URI", lr: listenRequest{uris: []string{"mark://team/"}, meta: map[string]any{mcpwatch.MetaSince: map[string]string{"mark://team/x/": "e:1"}}}, want: protocol.StatusBadRequest},
		{name: "since not an object", lr: listenRequest{uris: []string{"mark://team/"}, meta: map[string]any{mcpwatch.MetaSince: "e:1"}}, want: protocol.StatusBadRequest},
		{name: "coalesce", lr: listenRequest{uris: []string{"mark://team/"}, meta: map[string]any{mcpwatch.MetaCoalesce: "all"}}, want: protocol.StatusBadRequest},
		{name: "bearer without expiry", lr: listenRequest{bearer: "no-expiry", uris: []string{"mark://team/"}}, want: protocol.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lr := tt.lr
			lr.bearer = cmp.Or(lr.bearer, "alice")
			expectRefused(t, r.url, lr, tt.want)
		})
	}
	r.waitSubscriptions(t, 0)
}

// The per-subject cap counts open subscriptions across listens and frees
// them when a listen ends.
func TestListenSubjectCap(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	r.gateway.subs.maxPerSubject = 2
	first := mustListen(t, r.url, listenRequest{bearer: "alice", uris: []string{"mark://team/a/", "mark://team/b/"}})
	expectRefused(t, r.url, listenRequest{bearer: "alice", uris: []string{"mark://team/c/"}}, protocol.StatusRateLimited)
	other, refused := openListen(t, r.url, listenRequest{bearer: "bob", uris: []string{"mark://team/c/"}})
	if refused != nil {
		t.Fatalf("another subject's listen refused: %+v", refused)
	}
	other.cancel()
	first.cancel()
	r.waitSubscriptions(t, 0)
}

func TestLegacySubscribeIsRefused(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	initR := mcpRequest(t, r.url, "alice", "", initializeRequest(1))
	if sub, _ := initR.Result["capabilities"].(map[string]any)["resources"].(map[string]any)["subscribe"].(bool); !sub {
		t.Fatalf("initialize capabilities = %v, want resources.subscribe", initR.Result["capabilities"])
	}
	resp := mcpRequest(t, r.url, "alice", initR.Headers[mcpSessionHeader], map[string]any{"jsonrpc": "2.0", "id": 2, "method": "resources/subscribe", "params": map[string]any{"uri": "mark://team/a.md"}})
	if msg, _ := resp.Error["message"].(string); !strings.Contains(msg, protocol.StatusBadRequest+": resource subscriptions need protocol 2026-07-28") {
		t.Fatalf("resources/subscribe = %s, want the refusal naming subscriptions/listen", resp.RawBody)
	}
}

// On the memory gateway identity is world: a tenant subscribes to its own
// world only, and a refusal never says whether another tenant's world exists.
func TestMemoryListenAdmitsOnlyTheCallersWorld(t *testing.T) {
	t.Parallel()
	r := memoryReplica(t)
	l := mustListen(t, r.url, listenRequest{bearer: "alice", uris: []string{"mark://alice-w/notes/"}})
	r.publishIn(t, "alice-w", "/notes/a.md")
	if uri, _ := l.update(t, 5*time.Second); uri != "mark://alice-w/notes/a.md" {
		t.Fatalf("update = %s, want alice's note", uri)
	}

	tests := []struct {
		name, bearer, foreign string
		uris                  []string
	}{
		{name: "another identity on a tenant's world", bearer: "bob", foreign: "alice-w", uris: []string{"mark://alice-w/"}},
		{name: "own world and another's", bearer: "alice", foreign: "bob-w", uris: []string{"mark://alice-w/", "mark://bob-w/notes/"}},
		{name: "a world that does not exist", bearer: "alice", foreign: "nobody-w", uris: []string{"mark://nobody-w/"}},
		{name: "an identity without a world", bearer: "carol", foreign: "alice-w", uris: []string{"mark://alice-w/"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refused := expectRefused(t, r.url, listenRequest{bearer: tt.bearer, uris: tt.uris}, protocol.StatusNotPermitted)
			if strings.Contains(refused.message, tt.foreign) {
				t.Errorf("refusal %q names the world %s asked for", refused.message, tt.foreign)
			}
		})
	}
	l.cancel()
	r.waitSubscriptions(t, 0)
}

// fakeSession is an MCP session whose notification channel a test drains.
type fakeSession struct{ ch chan mcp.JSONRPCNotification }

func (s *fakeSession) Initialize()                                         {}
func (s *fakeSession) Initialized() bool                                   { return true }
func (s *fakeSession) NotificationChannel() chan<- mcp.JSONRPCNotification { return s.ch }
func (s *fakeSession) SessionID() string                                   { return "" }

// attachFake admits a listen on uris as alice and serves it into session as
// if acknowledged.
func attachFake(t *testing.T, g *Gateway, session *fakeSession, uris ...string) {
	t.Helper()
	claims := brokertest.AliceClaims()
	claims.Expiry = time.Now().Add(time.Hour)
	ctx, cancel := context.WithCancel(core.CtxWithClaims(context.Background(), &claims))
	t.Cleanup(cancel)
	ctx = g.mcpServer.WithContext(ctx, session)
	var req subscribeRequest
	req.Params.Notifications.ResourceSubscriptions = uris
	stream := &listenStream{cancel: cancel, acked: make(chan struct{})}
	close(stream.acked)
	l, err := g.subs.attach(ctx, 1, &req, stream)
	if err != nil {
		t.Fatal(err)
	}
	go g.subs.serve(ctx, l)
}

// A listen whose client stops reading holds the world nothing: its watch
// queue overflows and reopens from its cursor while another listen keeps
// receiving, and once read again it has lost nothing.
func TestSlowListenLosesNothingWhileOthersReceive(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	r.gateway.subs.queue = 4
	const total = 20
	slow := &fakeSession{ch: make(chan mcp.JSONRPCNotification, 1)}
	fast := &fakeSession{ch: make(chan mcp.JSONRPCNotification, total)}
	attachFake(t, r.gateway, slow, "mark://team/s/")
	attachFake(t, r.gateway, fast, "mark://team/s/")
	for i := range total {
		r.publish(t, "/s/"+strconv.Itoa(i)+".md")
		select {
		case n := <-fast.ch:
			if uri := n.Params.AdditionalFields["uri"]; uri != "mark://team/s/"+strconv.Itoa(i)+".md" {
				t.Fatalf("fast listen update %d = %v", i, uri)
			}
		case <-time.After(latencyBudget):
			t.Fatalf("fast listen got nothing for event %d within %v while the slow one was not read", i, latencyBudget)
		}
	}
	for i := range total {
		select {
		case n := <-slow.ch:
			if uri := n.Params.AdditionalFields["uri"]; uri != "mark://team/s/"+strconv.Itoa(i)+".md" {
				t.Fatalf("slow listen update %d = %v, want every event in order", i, uri)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("slow listen lost events after %d", i)
		}
	}
}

func TestNoticeShapes(t *testing.T) {
	t.Parallel()
	l := &listen{id: "s1"}
	sub := &subscription{uri: "mark://team/docs/", world: "team"}
	cursor := protocol.Cursor{Epoch: "w", Seq: 9}
	tests := []struct {
		name     string
		notice   fetch.Notice
		wantURI  string
		wantMeta map[string]any
	}{
		{
			name:    "event on the changed document",
			notice:  fetch.Notice{Event: protocol.WatchEvent{Cursor: cursor, Path: "/docs/a.md", Version: 3, Op: protocol.OpAppend, Agent: "claude-code"}},
			wantURI: "mark://team/docs/a.md",
			wantMeta: map[string]any{mcp.MetaKeySubscriptionID: "s1", mcpwatch.MetaSubscription: sub.uri, mcpwatch.MetaCursor: "w:9",
				mcpwatch.MetaPath: "/docs/a.md", mcpwatch.MetaVersion: 3, mcpwatch.MetaOp: protocol.OpAppend, mcpwatch.MetaAgent: "claude-code"},
		},
		{
			name:     "resync on the subscribed URI",
			notice:   fetch.Notice{Resync: true, Event: protocol.WatchEvent{Cursor: cursor}},
			wantURI:  sub.uri,
			wantMeta: map[string]any{mcp.MetaKeySubscriptionID: "s1", mcpwatch.MetaSubscription: sub.uri, mcpwatch.MetaCursor: "w:9", mcpwatch.MetaResync: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := l.notice(sub, tt.notice)
			if n.Method != string(mcp.MethodNotificationResourceUpdated) || n.Params.AdditionalFields["uri"] != tt.wantURI {
				t.Fatalf("notification %s %v, want resources/updated on %s", n.Method, n.Params.AdditionalFields, tt.wantURI)
			}
			if fmt.Sprint(n.Params.Meta) != fmt.Sprint(tt.wantMeta) {
				t.Errorf("meta = %v, want %v", n.Params.Meta, tt.wantMeta)
			}
			wire, err := json.Marshal(n)
			if err != nil || !bytes.Contains(wire, []byte(`"_meta":{`)) {
				t.Errorf("wire %s %v, want _meta kept", wire, err)
			}
		})
	}
	if got := terminalStatus(&fetch.StatusError{Status: protocol.StatusNotPermitted}); got != protocol.StatusNotPermitted {
		t.Errorf("not-permitted ends as %s", got)
	}
	if got := terminalStatus(errors.New("world gone")); got != protocol.StatusClosing {
		t.Errorf("another end reads as %s, want closing", got)
	}
}

// The open-subscription count keeps one entry per subject with a listen
// open: churn through many subjects leaves nothing behind.
func TestSubscriptionCountsTrackLiveSubjects(t *testing.T) {
	s := &subscriptions{maxPerSubject: 256, open: map[string]int{}}
	subject := func(n int) string {
		return core.IdentityKey("https://accounts.google.com", fmt.Sprintf("1174%017d", n))
	}
	const warmup, cycles = 100, 20000
	for n := range warmup {
		s.reserve(subject(n), 2)
		s.release(subject(n), 2)
	}
	growth := memtest.Retained(func() {
		for n := warmup; n < warmup+cycles; n++ {
			if !s.reserve(subject(n), 2) {
				t.Fatal("reserve refused under the cap")
			}
			s.release(subject(n), 1)
			s.release(subject(n), 1)
		}
	})
	runtime.KeepAlive(s)
	t.Logf("heap grew %d bytes", growth)
	if len(s.open) != 0 || growth > 64*1024 {
		t.Errorf("after %d subjects came and went: %d entries, heap grew %d bytes", cycles, len(s.open), growth)
	}
}

// The library's client stack, mcp-go's streamable HTTP client on its default
// protocol, discovers the capability and receives updates with their _meta.
func TestMCPGoClientListens(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	c, discovered := readerClient(t, r, "alice", "")
	got := make(chan mcp.JSONRPCNotification, 16)
	c.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == string(mcp.MethodNotificationResourceUpdated) {
			got <- n
		}
	})
	ctx := context.Background()
	if discovered.ProtocolVersion != mcp.ProtocolVersion20260728 || discovered.Capabilities.Resources == nil || !discovered.Capabilities.Resources.Subscribe {
		t.Fatalf("negotiated %s with %+v, want 2026-07-28 and resources.subscribe", discovered.ProtocolVersion, discovered.Capabilities.Resources)
	}
	stop, err := c.ListenAsync(ctx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{"mark://team/m/"}}, func(err error) { t.Errorf("listen: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	r.waitSubscriptions(t, 1)
	r.publish(t, "/m/one.md")
	select {
	case n := <-got:
		if n.Params.AdditionalFields["uri"] != "mark://team/m/one.md" || n.Params.Meta[mcpwatch.MetaSubscription] != "mark://team/m/" {
			t.Fatalf("update = %v %v", n.Params.AdditionalFields, n.Params.Meta)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no update reached the mcp-go client")
	}
}

// openCount is every subject's open subscriptions.
func (s *subscriptions) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, n := range s.open {
		total += n
	}
	return total
}

// waitSubscriptions waits until n subscriptions are open.
func (r *watchReplica) waitSubscriptions(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.gateway.subs.openCount() != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d subscriptions open, want %d", r.gateway.subs.openCount(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
