package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/mcpwatch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// WATCH on the MCP binding (ADR 0047, SPEC §6.8.1): a 2026-07-28
// subscriptions/listen stream naming mark:// URIs is the watch.

// WorldWatcher opens in-process watches of local worlds; *Composite is one.
type WorldWatcher interface {
	Watch(ctx context.Context, r fetch.WatchRequest) (*fetch.Watch, error)
}

const (
	defaultMaxURIsPerListen  = 64
	defaultMaxSubsPerSubject = 256
	defaultListenGrace       = 30 * time.Second
	defaultSubscriptionQueue = 64
)

// errLegacySubscribe answers resources/subscribe, which the binding defers.
var errLegacySubscribe = errors.New(protocol.StatusBadRequest + ": resource subscriptions need protocol 2026-07-28 subscriptions/listen")

// subscriptions admits listen streams and holds the count of open
// subscriptions per subject; an entry leaves the map at zero.
type subscriptions struct {
	// source watches only this gateway's profile (scopeWatcher).
	source WorldWatcher
	// admitURI is the one world rule reads apply (Gateway.admitResourceURI).
	admitURI      func(context.Context, string) (context.Context, core.WorldConfig, error)
	issuer        string
	log           *slog.Logger
	maxPerListen  int
	maxPerSubject int
	grace         time.Duration
	// queue bounds each watch's notices; a full one reopens from its cursor.
	queue int

	mu   sync.Mutex
	open map[string]int
}

func newSubscriptions(deps *Deps, admitURI func(context.Context, string) (context.Context, core.WorldConfig, error)) *subscriptions {
	return &subscriptions{
		source: scopeWatcher(deps.Worlds, deps.Watches), admitURI: admitURI, issuer: deps.Issuer, log: deps.Log,
		maxPerListen: defaultMaxURIsPerListen, maxPerSubject: defaultMaxSubsPerSubject,
		grace: defaultListenGrace, queue: defaultSubscriptionQueue, open: map[string]int{},
	}
}

// reserve counts n more subscriptions for subject, or refuses past the cap.
func (s *subscriptions) reserve(subject string, n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[subject]+n > s.maxPerSubject {
		return false
	}
	s.open[subject] += n
	return true
}

func (s *subscriptions) release(subject string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if left := s.open[subject] - n; left > 0 {
		s.open[subject] = left
		return
	}
	delete(s.open, subject)
}

// abandon closes a listen that never served and returns its reservation.
func (s *subscriptions) abandon(l *listen, reserved int) {
	l.close()
	s.release(l.subject, reserved)
}

// listenStream is the HTTP side of one subscriptions/listen request: the
// cancel that ends it, and acked, closed once mcp-go wrote the acknowledgement.
type listenStream struct {
	cancel context.CancelFunc
	acked  chan struct{}
}

type listenStreamKey struct{}

var ackMarker = []byte(`"method":"` + mcp.MethodNotificationSubscriptionsAcknowledged + `"`)

// listenStreams gives each listen request a cancel the gateway can end it
// with, and watches its response for the acknowledgement. The Mcp-Method
// header is safe to route on: mcp-go refuses a request it does not match.
func listenStreams(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get(mcp.HeaderMethod) != string(mcp.MethodSubscriptionsListen) {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stream := &listenStream{cancel: cancel, acked: make(chan struct{})}
		next.ServeHTTP(&ackWriter{ResponseWriter: w, stream: stream}, r.WithContext(context.WithValue(ctx, listenStreamKey{}, stream)))
	})
}

// ackWriter closes acked on the write carrying the acknowledgement. mcp-go
// writes one SSE event per Write, under its own lock, and nothing precedes
// the acknowledgement.
type ackWriter struct {
	http.ResponseWriter
	stream *listenStream
}

func (w *ackWriter) Write(p []byte) (int, error) {
	select {
	case <-w.stream.acked:
	default:
		if bytes.Contains(p, ackMarker) {
			close(w.stream.acked)
		}
	}
	return w.ResponseWriter.Write(p)
}

func (w *ackWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *ackWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// subscribeRequest is the part of a listen or subscribe request admission reads.
type subscribeRequest struct {
	Method mcp.MCPMethod `json:"method"`
	Params struct {
		Meta          map[string]json.RawMessage `json:"_meta"`
		Notifications mcp.SubscriptionFilter     `json:"notifications"`
	} `json:"params"`
}

// subscription is one subscribed URI of a listen and, once open, its watch.
type subscription struct {
	uri, world, path string
	watch            *fetch.Watch
}

// listen is one admitted stream: notifications go to session, tagged id.
type listen struct {
	stream  *listenStream
	session mcpserver.ClientSession
	id      any
	subject string
	subs    []*subscription
}

// A method named as a JSON string: document text cannot match it, since a
// quote inside a JSON string is escaped.
var (
	quotedListen    = []byte(`"` + string(mcp.MethodSubscriptionsListen) + `"`)
	quotedSubscribe = []byte(`"` + string(mcp.MethodResourcesSubscribe) + `"`)
)

// admit is the OnRequestInitialization hook, run on every request: it refuses
// a legacy subscribe and admits a listen's URIs as reads before attaching
// them. The in-process door skips the listener gate: this is the only one.
func (s *subscriptions) admit(ctx context.Context, id, message any) error {
	raw, ok := message.(json.RawMessage)
	stream, _ := ctx.Value(listenStreamKey{}).(*listenStream)
	if !ok || (stream == nil && !bytes.Contains(raw, quotedSubscribe) && !bytes.Contains(raw, quotedListen)) {
		return nil
	}
	var req subscribeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil // mcp-go answers the parse error
	}
	switch {
	case req.Method == mcp.MethodResourcesSubscribe:
		return errLegacySubscribe
	case req.Method != mcp.MethodSubscriptionsListen || len(req.Params.Notifications.ResourceSubscriptions) == 0:
		return nil
	}
	if !modernRequest(req.Params.Meta) {
		return nil // mcp-go refuses a listen of an earlier protocol version
	}
	if stream == nil {
		// Acknowledged, it would deliver nothing; refuse it instead.
		return fmt.Errorf("%s: subscriptions need the gateway's HTTP route", protocol.StatusServerError)
	}
	l, err := s.attach(ctx, id, &req, stream)
	if err != nil {
		s.log.InfoContext(ctx, "broker: mcp listen refused", "err", err)
		return err
	}
	go s.serve(ctx, l)
	return nil
}

func modernRequest(meta map[string]json.RawMessage) bool {
	var version string
	if err := json.Unmarshal(meta[mcp.MetaKeyProtocolVersion], &version); err != nil {
		return false
	}
	return mcp.IsModernProtocol(version)
}

// attach admits every URI of req and opens its watch: all or none.
func (s *subscriptions) attach(ctx context.Context, id any, req *subscribeRequest, stream *listenStream) (*listen, error) {
	claims, ok := core.ClaimsFromCtx(ctx)
	if !ok {
		return nil, fmt.Errorf("%s: no verified identity", protocol.StatusUnauthorized)
	}
	if claims.Expiry.IsZero() {
		return nil, fmt.Errorf("%s: bearer carries no expiry", protocol.StatusUnauthorized)
	}
	session := mcpserver.ClientSessionFromContext(ctx)
	if session == nil {
		return nil, fmt.Errorf("%s: listen without a session", protocol.StatusServerError)
	}
	subs, err := s.targets(ctx, req.Params.Notifications.ResourceSubscriptions)
	if err != nil {
		return nil, err
	}
	opts, err := listenOptions(req.Params.Meta, subs)
	if err != nil {
		return nil, err
	}
	l := &listen{stream: stream, session: session, id: id, subject: core.IdentityKey(s.issuer, claims.Subject)}
	if !s.reserve(l.subject, len(subs)) {
		return nil, fmt.Errorf("%s: more than %d open subscriptions", protocol.StatusRateLimited, s.maxPerSubject)
	}
	// Reads carry no token, so neither does the watch: it sees what a read
	// sees. The grant bounds it by the bearer's expiry, which the world keeps.
	watchCtx := protocol.WithGrant(ctx, protocol.Grant{Label: claims.Email, Expires: claims.Expiry})
	for _, sub := range subs {
		w, err := s.source.Watch(watchCtx, fetch.WatchRequest{Host: sub.world, Path: sub.path, Since: opts.since[sub.uri], Coalesce: opts.coalesce, Queue: s.queue})
		if err != nil {
			s.abandon(l, len(subs))
			return nil, watchRefusal(sub.uri, err)
		}
		sub.watch = w
		l.subs = append(l.subs, sub)
	}
	s.log.InfoContext(ctx, "broker: mcp listen admitted", "subject", core.HashSubject(claims.Subject), "subscriptions", len(subs))
	return l, nil
}

// targets parses each URI and admits it as a read would; duplicates collapse.
// The world's existence is the scoped watcher's to judge, after admission, so
// a memory caller never learns of another tenant's world.
func (s *subscriptions) targets(ctx context.Context, uris []string) ([]*subscription, error) {
	if len(uris) > s.maxPerListen {
		return nil, fmt.Errorf("%s: more than %d URIs in one listen", protocol.StatusRateLimited, s.maxPerListen)
	}
	seen := map[string]bool{}
	var subs []*subscription
	for _, raw := range uris {
		world, path, err := parseToolURL(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", protocol.StatusBadRequest, err)
		}
		// The admitted ctx carries the tenant's world to the next URI.
		if ctx, _, err = s.admitURI(ctx, raw); err != nil {
			return nil, admissionRefusal(err)
		}
		uri := mcpwatch.SubscriptionURI(world, path)
		if !seen[uri] {
			seen[uri] = true
			subs = append(subs, &subscription{uri: uri, world: world, path: path})
		}
	}
	return subs, nil
}

// admissionRefusal names the status of a URI the one world rule refused:
// provisioning still under way is retry-later, anything else final.
func admissionRefusal(err error) error {
	if errors.Is(err, errProvisioning) || errors.Is(err, errProvisionFailed) {
		return fmt.Errorf("%s: %w", protocol.StatusServerError, err)
	}
	return fmt.Errorf("%s: %w", protocol.StatusNotPermitted, err)
}

// watchOptions are a listen's demarkus.io/ request keys.
type watchOptions struct {
	since    map[string]protocol.Cursor // by subscribed URI
	coalesce bool
}

// listenOptions reads demarkus.io/since (subscribed URI to cursor) and
// demarkus.io/coalesce. A cursor for a URI not subscribed is refused.
func listenOptions(meta map[string]json.RawMessage, subs []*subscription) (watchOptions, error) {
	opts := watchOptions{since: map[string]protocol.Cursor{}}
	if raw, ok := meta[mcpwatch.MetaSince]; ok {
		var byURI map[string]string
		if err := json.Unmarshal(raw, &byURI); err != nil {
			return watchOptions{}, fmt.Errorf("%s: %s is not an object of cursors", protocol.StatusBadRequest, mcpwatch.MetaSince)
		}
		subscribed := map[string]bool{}
		for _, sub := range subs {
			subscribed[sub.uri] = true
		}
		for rawURI, value := range byURI {
			world, path, err := parseToolURL(rawURI)
			uri := mcpwatch.SubscriptionURI(world, path)
			if err != nil || !subscribed[uri] {
				return watchOptions{}, fmt.Errorf("%s: %s names %q, which is not subscribed", protocol.StatusBadRequest, mcpwatch.MetaSince, rawURI)
			}
			cursor, err := protocol.ParseCursor(value)
			if err != nil {
				return watchOptions{}, fmt.Errorf("%s: %s for %q: %w", protocol.StatusBadRequest, mcpwatch.MetaSince, rawURI, err)
			}
			opts.since[uri] = cursor
		}
	}
	if raw, ok := meta[mcpwatch.MetaCoalesce]; ok {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || value != "path" {
			return watchOptions{}, fmt.Errorf("%s: %s must be \"path\"", protocol.StatusBadRequest, mcpwatch.MetaCoalesce)
		}
		opts.coalesce = true
	}
	return opts, nil
}

// watchRefusal names the §7 status of a watch that would not open.
func watchRefusal(uri string, err error) error {
	var refused *fetch.StatusError
	var unknown *errWorldNotFound
	switch {
	case errors.As(err, &refused):
		return fmt.Errorf("%s: %s: %w", refused.Status, uri, err)
	case errors.As(err, &unknown), errors.Is(err, errWorldNotLocal):
		return fmt.Errorf("%s: %s: %w", protocol.StatusNotFound, uri, err)
	}
	return fmt.Errorf("%s: %s: %w", protocol.StatusServerError, uri, err)
}

func (l *listen) close() {
	for _, sub := range l.subs {
		sub.watch.Close()
	}
}

// serve delivers once mcp-go has acknowledged the listen and ends the stream
// a grace after its last subscription, so terminal notices arrive. A listen
// mcp-go refused is never acknowledged; its context ends the watches.
func (s *subscriptions) serve(ctx context.Context, l *listen) {
	select {
	case <-l.stream.acked:
	case <-ctx.Done():
		s.abandon(l, len(l.subs))
		return
	}
	var wg sync.WaitGroup
	for _, sub := range l.subs {
		wg.Go(func() {
			defer s.release(l.subject, 1)
			defer sub.watch.Close()
			s.pump(ctx, l, sub)
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	timer := time.NewTimer(s.grace)
	defer timer.Stop()
	select {
	case <-timer.C:
		l.stream.cancel()
	case <-ctx.Done():
	}
}

// pump turns a subscription's notices into notifications, then its end, such
// as the world's unauthorized at the bearer's expiry, into a terminal notice.
func (s *subscriptions) pump(ctx context.Context, l *listen, sub *subscription) {
	for {
		n, err := sub.watch.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			status := terminalStatus(err)
			if status == protocol.StatusClosing {
				s.log.WarnContext(ctx, "broker: mcp subscription ended", "uri", sub.uri, "err", err)
			}
			l.offer(ctx, l.terminal(sub, status))
			return
		}
		if !l.offer(ctx, l.notice(sub, n)) {
			return
		}
	}
}

// offer queues n on the listen's session. A full channel blocks this pump
// alone; its watch's bounded queue then reopens from its cursor, so the world
// never waits and nothing is lost. False once ctx ended.
func (l *listen) offer(ctx context.Context, n mcp.JSONRPCNotification) bool {
	select {
	case l.session.NotificationChannel() <- n:
		return true
	case <-ctx.Done():
		return false
	}
}

// notice is an event, on the changed document's URI, or a resync, on the
// subscribed one. SendNotification* would drop _meta (AdditionalFields).
func (l *listen) notice(sub *subscription, n fetch.Notice) mcp.JSONRPCNotification {
	meta := l.meta(sub, n.Event.Cursor)
	if n.Resync {
		meta[mcpwatch.MetaResync] = true
		return resourceUpdated(sub.uri, meta)
	}
	ev := n.Event
	meta[mcpwatch.MetaPath], meta[mcpwatch.MetaVersion], meta[mcpwatch.MetaOp] = ev.Path, ev.Version, ev.Op
	if ev.Hash != "" {
		meta[mcpwatch.MetaHash] = ev.Hash
	}
	if ev.Agent != "" {
		meta[mcpwatch.MetaAgent] = ev.Agent
	}
	if ev.User != "" {
		meta[mcpwatch.MetaUser] = ev.User
	}
	return resourceUpdated(mcpwatch.SubscriptionURI(sub.world, ev.Path), meta)
}

// terminalStatus is a credential verdict as itself; anything else ends the
// subscription as closing, from whose cursor the client resumes.
func terminalStatus(err error) string {
	var refused *fetch.StatusError
	if errors.As(err, &refused) && (refused.Status == protocol.StatusUnauthorized || refused.Status == protocol.StatusNotPermitted) {
		return refused.Status
	}
	return protocol.StatusClosing
}

// terminal ends a subscription with status and the last cursor delivered.
func (l *listen) terminal(sub *subscription, status string) mcp.JSONRPCNotification {
	meta := l.meta(sub, sub.watch.Cursor())
	meta[mcpwatch.MetaStatus] = status
	return resourceUpdated(sub.uri, meta)
}

// meta holds room for a full event's keys.
func (l *listen) meta(sub *subscription, cursor protocol.Cursor) map[string]any {
	meta := make(map[string]any, 10)
	meta[mcp.MetaKeySubscriptionID] = l.id
	meta[mcpwatch.MetaSubscription] = sub.uri
	meta[mcpwatch.MetaCursor] = cursor.String()
	return meta
}

func resourceUpdated(uri string, meta map[string]any) mcp.JSONRPCNotification {
	return mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Notification: mcp.Notification{
			Method: string(mcp.MethodNotificationResourceUpdated),
			Params: mcp.NotificationParams{Meta: meta, AdditionalFields: map[string]any{"uri": uri}},
		},
	}
}
