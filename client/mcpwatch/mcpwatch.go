// Package mcpwatch follows a WATCH subscription over the MCP binding (SPEC
// §6.8.1) on an existing mcp-go client: a subscriptions/listen stream that
// renews with fresh bearers and resumes from its cursor, read as a fetch.Watch.
package mcpwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// The binding's _meta keys (SPEC §6.8.1), shared with the gateway that
// writes them.
const (
	MetaSince        = "demarkus.io/since"
	MetaCoalesce     = "demarkus.io/coalesce"
	MetaSubscription = "demarkus.io/subscription"
	MetaCursor       = "demarkus.io/cursor"
	MetaPath         = "demarkus.io/path"
	MetaVersion      = "demarkus.io/version"
	MetaOp           = "demarkus.io/op"
	MetaHash         = "demarkus.io/hash"
	MetaAgent        = "demarkus.io/agent"
	MetaUser         = "demarkus.io/user"
	MetaResync       = "demarkus.io/resync"
	MetaStatus       = "demarkus.io/status"
)

// SubscriptionURI is how both ends name a subscription to path in world:
// the world name lowercase, the path as the URI parser decoded it.
func SubscriptionURI(world, path string) string { return "mark://" + world + path }

const (
	defaultQueue     = 1024
	handshakeTimeout = 10 * time.Second
	// renewBefore is how long before its bearer lapses a stream is replaced;
	// a shorter-lived bearer renews at half its life.
	renewBefore  = time.Minute
	retryMin     = 100 * time.Millisecond
	retryMax     = 5 * time.Second
	closingPause = 500 * time.Millisecond
)

// Listener routes one mcp-go client's subscription notifications to the
// streams open on it. Make one per client, once: mcp-go keeps every
// notification handler for the client's life.
type Listener struct {
	client  *mcpclient.Client
	ids     atomic.Uint64
	mu      sync.Mutex
	streams map[string]*stream // by listen request id, while open
}

// NewListener routes c's notifications. c must be started and initialized.
func NewListener(c *mcpclient.Client) *Listener {
	l := &Listener{client: c, streams: map[string]*stream{}}
	c.OnNotification(l.dispatch)
	return l
}

func (l *Listener) dispatch(n mcp.JSONRPCNotification) {
	id, _ := n.Params.Meta[mcp.MetaKeySubscriptionID].(string)
	l.mu.Lock()
	s := l.streams[id]
	l.mu.Unlock()
	if s == nil {
		return
	}
	switch n.Method {
	case mcp.MethodNotificationSubscriptionsAcknowledged:
		s.acknowledge(n)
	case string(mcp.MethodNotificationResourceUpdated):
		s.watch.receive(s, n)
	}
}

// Session is what one stream opens on: a client's listener, and when the
// bearer it sends lapses. Zero Expires renews only once the server ends it.
type Session struct {
	Listener *Listener
	Expires  time.Time
}

// Source yields the session for every open and renewal. Its error ends the
// watch; the caller reopens from Cursor once a session is available again.
type Source func(ctx context.Context) (Session, error)

// Request is one subscription: URI is mark://<world><path>, a trailing "/"
// watching the subtree. Since, Coalesce and Queue are as in fetch.WatchRequest.
type Request struct {
	URI      string
	Since    protocol.Cursor
	Coalesce bool
	Queue    int
}

// Watch is one subscription that outlives its streams, read like a
// fetch.Watch: Next, Cursor, Err, Close.
type Watch struct {
	source   Source
	uri      string
	coalesce bool

	notices chan fetch.Notice
	room    chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc

	mu sync.Mutex
	// stream is where the next stream resumes from; consumed is the cursor of
	// the last notice Next returned, which may lag behind what is queued.
	stream, consumed protocol.Cursor
	// owed is a resync not yet queued for a reopen from no cursor, which
	// starts at the head and so never sends what landed during the gap.
	owed    bool
	expires time.Time
	err     error
}

// Open subscribes and returns once the server has acknowledged, or with its
// refusal as a *fetch.StatusError. Notices arrive through Next until ctx
// ends or the subscription ends for good.
func Open(ctx context.Context, source Source, r Request) (*Watch, error) {
	target, err := links.ParseMark(r.URI)
	if err != nil {
		return nil, fmt.Errorf("subscribe %q: %w", r.URI, err)
	}
	queue := r.Queue
	if queue < 0 {
		return nil, fmt.Errorf("subscribe %q: queue %d is negative", r.URI, queue)
	}
	if queue == 0 {
		queue = defaultQueue
	}
	w := &Watch{
		source: source, uri: SubscriptionURI(target.Hostname(), target.Path), coalesce: r.Coalesce,
		notices: make(chan fetch.Notice, queue), room: make(chan struct{}, 1), done: make(chan struct{}),
		stream: r.Since, consumed: r.Since,
	}
	// Every stream, the first included, lives as long as the watch.
	runCtx, cancel := context.WithCancel(ctx)
	s, err := w.open(runCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	w.cancel = cancel
	go w.run(runCtx, s)
	return w, nil
}

// Next returns the next notice, or the error that ended the watch.
func (w *Watch) Next(ctx context.Context) (fetch.Notice, error) {
	select {
	case n := <-w.notices:
		return w.consume(n), nil
	default:
	}
	select {
	case n := <-w.notices:
		return w.consume(n), nil
	case <-w.done:
		return fetch.Notice{}, w.Err()
	case <-ctx.Done():
		return fetch.Notice{}, ctx.Err()
	}
}

func (w *Watch) consume(n fetch.Notice) fetch.Notice {
	w.mu.Lock()
	w.consumed = n.Event.Cursor
	w.mu.Unlock()
	select {
	case w.room <- struct{}{}:
	default:
	}
	return n
}

// Cursor is where a later watch resumes without missing anything: the
// cursor of the last notice Next returned.
func (w *Watch) Cursor() protocol.Cursor {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.consumed
}

// Err is why the watch ended, once Next has reported it.
func (w *Watch) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Close ends the watch and its stream.
func (w *Watch) Close() {
	w.cancel()
	<-w.done
}

func (w *Watch) finish(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
	close(w.done)
}

func (w *Watch) streamCursor() protocol.Cursor {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stream
}

// Signals from a stream's notifications to the loop following it, beside
// the terminal statuses.
const (
	signalOverflow  = "overflow"
	signalMalformed = "malformed"
)

// stream is one subscriptions/listen request.
type stream struct {
	watch   *Watch
	cancel  context.CancelFunc
	acked   chan struct{}
	ackOnce sync.Once
	granted bool // set before acked closes
	ended   chan struct{}
	endErr  error // set before ended closes
	signal  chan string
	// dead stops a stream's later notices once it overflowed, ended or was
	// replaced; guarded by the watch's mu.
	dead      bool
	delivered bool
	renewAt   time.Time
}

func (s *stream) acknowledge(n mcp.JSONRPCNotification) {
	s.ackOnce.Do(func() {
		var granted mcp.SubscriptionFilter
		if raw, err := json.Marshal(n.Params.AdditionalFields["notifications"]); err == nil && json.Unmarshal(raw, &granted) == nil {
			s.granted = slices.Contains(granted.ResourceSubscriptions, s.watch.uri)
		}
		close(s.acked)
	})
}

// raise hands the loop a signal without blocking the transport's reader;
// the first one wins.
func (s *stream) raise(signal string) {
	select {
	case s.signal <- signal:
	default:
	}
}

// stop ends the stream; its later notices are dropped.
func (s *stream) stop() {
	s.watch.mu.Lock()
	s.dead = true
	s.watch.mu.Unlock()
	s.cancel()
}

// receive runs on the transport's reader: it queues an event or resync
// without blocking, and drops what a renewal's overlap repeats.
func (w *Watch) receive(s *stream, n mcp.JSONRPCNotification) {
	meta := n.Params.Meta
	if meta[MetaSubscription] != w.uri {
		return
	}
	cursor, err := protocol.ParseCursor(text(meta, MetaCursor))
	if err != nil {
		s.raise(signalMalformed)
		return
	}
	if status := text(meta, MetaStatus); status != "" {
		s.raise(status)
		return
	}
	notice, err := noticeOf(meta, cursor)
	if err != nil {
		s.raise(signalMalformed)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if s.dead || (!notice.Resync && !after(cursor, w.stream)) {
		return
	}
	if !notice.Resync && !protocol.IsKnownOp(notice.Event.Op) {
		w.stream = cursor // skipped, as the protocol asks, but passed
		return
	}
	// The owed resync goes first: an event consumed ahead of it would move
	// Cursor past the gap.
	if !w.payOwed(s) {
		return
	}
	select {
	case w.notices <- notice:
		w.stream, s.delivered = cursor, true
	default:
		// Full: nothing after this is taken from s, and the next stream
		// resumes from the last queued cursor, so nothing is lost.
		s.dead = true
		s.raise(signalOverflow)
	}
}

// after reports whether cursor is past last within one epoch; another epoch
// is always new.
func after(cursor, last protocol.Cursor) bool {
	return last.IsZero() || cursor.Epoch != last.Epoch || cursor.Seq > last.Seq
}

// noticeOf decodes through the QUIC binding's event block, so both bindings
// validate an event alike.
func noticeOf(meta map[string]any, cursor protocol.Cursor) (fetch.Notice, error) {
	if resync, _ := meta[MetaResync].(bool); resync {
		return fetch.Notice{Resync: true, Event: protocol.WatchEvent{Cursor: cursor}}, nil
	}
	version, ok := meta[MetaVersion].(float64)
	if !ok || version != float64(int(version)) {
		return fetch.Notice{}, errors.New("event version is not an integer")
	}
	event, err := protocol.WatchBlock{Metadata: map[string]string{
		"cursor": cursor.String(), "path": text(meta, MetaPath), "version": strconv.Itoa(int(version)),
		"op": text(meta, MetaOp), "hash": text(meta, MetaHash), protocol.MetaAgent: text(meta, MetaAgent), protocol.MetaUser: text(meta, MetaUser),
	}}.Event()
	if err != nil {
		return fetch.Notice{}, err
	}
	return fetch.Notice{Event: event}, nil
}

func text(meta map[string]any, key string) string {
	value, _ := meta[key].(string)
	return value
}

// run follows streams until the watch ends: it renews ahead of a lapsing
// bearer or at once on unauthorized, and reopens after a cut, a closing
// drain or an overflow from the stream cursor, or with a resync if none.
func (w *Watch) run(ctx context.Context, s *stream) {
	defer w.cancel()
	empty := retryMin
	for {
		next, pause, err := w.follow(ctx, s)
		if err != nil {
			w.finish(err)
			return
		}
		if next != nil {
			s = next
			continue
		}
		// A stream that ended carrying nothing reopens after a growing pause,
		// so a server ending every stream cannot spin the watch.
		if s.delivered {
			empty = retryMin
		} else {
			pause = max(pause, empty)
			empty = min(empty*2, retryMax)
		}
		if !sleep(ctx, pause) {
			w.finish(ctx.Err())
			return
		}
		w.mu.Lock()
		w.owed = w.owed || w.stream.IsZero()
		w.mu.Unlock()
		if s, err = w.reopen(ctx); err != nil {
			w.finish(err)
			return
		}
		// A quiet stream pays here; one that already delivered paid first.
		w.mu.Lock()
		w.payOwed(s)
		w.mu.Unlock()
	}
}

// payOwed queues the owed resync at the stream cursor, so Cursor never moves
// back. A full queue stops s like an overflow and keeps the debt for the next
// reopen, reporting false. Callers hold w.mu.
func (w *Watch) payOwed(s *stream) bool {
	if !w.owed {
		return true
	}
	select {
	case w.notices <- fetch.Notice{Resync: true, Event: protocol.WatchEvent{Cursor: w.stream}}:
		w.owed = false
		return true
	default:
		s.dead = true
		s.raise(signalOverflow)
		return false
	}
}

// follow waits on s until it ends. A renewal returns the stream that replaced
// it; otherwise the pause before reopening, or the error that ends the watch.
func (w *Watch) follow(ctx context.Context, s *stream) (next *stream, pause time.Duration, err error) {
	var renew <-chan time.Time
	if !s.renewAt.IsZero() {
		timer := time.NewTimer(time.Until(s.renewAt))
		defer timer.Stop()
		renew = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			s.stop()
			return nil, 0, ctx.Err()
		case <-s.ended:
			s.stop()
			return nil, retryMin, nil
		case signal := <-s.signal:
			s.stop()
			switch signal {
			case protocol.StatusUnauthorized:
				return nil, 0, nil // the bearer lapsed: reopen at once from a fresh session
			case protocol.StatusClosing:
				return nil, closingPause, nil
			case signalMalformed:
				return nil, retryMin, nil
			case signalOverflow:
				return nil, 0, w.waitRoom(ctx)
			default:
				return nil, 0, &fetch.StatusError{Status: signal}
			}
		case <-renew:
			renew = nil
			successor, err := w.open(ctx)
			switch {
			case err == nil:
				s.stop()
				return successor, 0, nil
			case retryable(err):
				// Keep s; it renews at once when the server ends it.
			default:
				s.stop()
				return nil, 0, err
			}
		}
	}
}

// waitRoom blocks until the queue has room again after an overflow.
func (w *Watch) waitRoom(ctx context.Context) error {
	for len(w.notices) == cap(w.notices) {
		select {
		case <-w.room:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// reopen opens a stream, retrying retry-later refusals and transport errors.
func (w *Watch) reopen(ctx context.Context) (*stream, error) {
	backoff := retryMin
	for {
		s, err := w.open(ctx)
		if err == nil || !retryable(err) || ctx.Err() != nil {
			return s, err
		}
		if !sleep(ctx, backoff) {
			return nil, ctx.Err()
		}
		backoff = min(backoff*2, retryMax)
	}
}

// Errors that end a watch whatever the retry policy.
var (
	errSource     = errors.New("listen source")
	errNotModern  = errors.New("resource subscriptions need MCP " + mcp.ProtocolVersion20260728)
	errNotOffered = errors.New("server offers no subscription to this URI")
)

// retryable is a retry-later refusal (SPEC §7) or a transport failure.
func retryable(err error) bool {
	var refused *fetch.StatusError
	if errors.As(err, &refused) {
		return refused.Status == protocol.StatusRateLimited || refused.Status == protocol.StatusServerError
	}
	return !errors.Is(err, errSource) && !errors.Is(err, errNotModern) && !errors.Is(err, errNotOffered) && !errors.Is(err, context.Canceled)
}

// open asks the source for a session and subscribes on it from the stream
// cursor, returning once the server acknowledged.
func (w *Watch) open(ctx context.Context) (*stream, error) {
	session, err := w.source(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSource, err)
	}
	l := session.Listener
	version := l.client.ProtocolVersion()
	if !mcp.IsModernProtocol(version) {
		return nil, fmt.Errorf("%w: the client negotiated %q", errNotModern, version)
	}
	s, err := l.start(ctx, w, version)
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(handshakeTimeout)
	defer timer.Stop()
	select {
	case <-s.acked:
		if !s.granted {
			s.stop()
			return nil, fmt.Errorf("subscribe %s: %w", w.uri, errNotOffered)
		}
	case <-s.ended:
		if s.endErr == nil {
			return nil, fmt.Errorf("subscribe %s: the stream ended before its acknowledgement", w.uri)
		}
		return nil, s.endErr
	case <-timer.C:
		s.stop()
		return nil, fmt.Errorf("subscribe %s: no acknowledgement within %v", w.uri, handshakeTimeout)
	case <-ctx.Done():
		s.stop()
		return nil, ctx.Err()
	}
	// Only Open, then run, ever opens: no lock for these two.
	s.renewAt = renewTime(session.Expires, w.expires, time.Now())
	w.expires = session.Expires
	return s, nil
}

// renewTime is when a stream whose bearer lapses at expires is replaced: zero
// when unknown, too close, or no fresher than the bearer it replaced.
func renewTime(expires, previous, now time.Time) time.Time {
	if expires.IsZero() || (!previous.IsZero() && !expires.After(previous)) {
		return time.Time{}
	}
	life := expires.Sub(now)
	if life <= 2*time.Second {
		return time.Time{}
	}
	return expires.Add(-min(renewBefore, life/2))
}

// start sends the listen request; the stream ends when it returns.
func (l *Listener) start(ctx context.Context, w *Watch, version string) (*stream, error) {
	meta := map[string]any{
		mcp.MetaKeyProtocolVersion:    version,
		mcp.MetaKeyClientCapabilities: l.client.GetClientCapabilities(),
	}
	if since := w.streamCursor(); !since.IsZero() {
		meta[MetaSince] = map[string]string{w.uri: since.String()}
	}
	if w.coalesce {
		meta[MetaCoalesce] = "path"
	}
	params := map[string]any{"_meta": meta, "notifications": map[string]any{"resourceSubscriptions": []string{w.uri}}}
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode listen: %w", err)
	}
	header := http.Header{}
	for name, value := range mcp.StandardHeaders(version, mcp.MethodSubscriptionsListen, encoded) {
		header.Set(name, value)
	}
	id := fmt.Sprintf("mcpwatch-%d", l.ids.Add(1))
	streamCtx, cancel := context.WithCancel(ctx)
	s := &stream{watch: w, cancel: cancel, acked: make(chan struct{}), ended: make(chan struct{}), signal: make(chan string, 1)}
	l.mu.Lock()
	l.streams[id] = s
	l.mu.Unlock()
	go func() {
		defer close(s.ended)
		// A refused or failed request must not stay a child of the watch's
		// context, or every retry would grow it.
		defer cancel()
		defer func() {
			l.mu.Lock()
			delete(l.streams, id)
			l.mu.Unlock()
		}()
		resp, err := l.client.GetTransport().SendRequest(streamCtx, transport.JSONRPCRequest{
			JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(id), Method: string(mcp.MethodSubscriptionsListen), Params: params, Header: header,
		})
		s.endErr = endError(resp, err)
	}()
	return s, nil
}

// endError is why a listen request returned: a refusal as a StatusError, a
// transport failure as is, nil for a stream the server closed cleanly.
func endError(resp *transport.JSONRPCResponse, err error) error {
	var denied *transport.AuthorizationRequiredError
	var deniedOAuth *transport.OAuthAuthorizationRequiredError
	switch {
	case errors.As(err, &denied), errors.As(err, &deniedOAuth):
		return &fetch.StatusError{Status: protocol.StatusUnauthorized, Message: err.Error()}
	case err != nil:
		return fmt.Errorf("listen: %w", err)
	case resp != nil && resp.Error != nil:
		return refusal(resp.Error.Message)
	}
	return nil
}

// refusal reads the §7 status a gateway puts first in its error message; a
// message without one is the server's bad request.
func refusal(message string) *fetch.StatusError {
	status, rest, ok := strings.Cut(message, ":")
	if ok && slices.Contains(refusalStatuses, status) {
		return &fetch.StatusError{Status: status, Message: strings.TrimSpace(rest)}
	}
	return &fetch.StatusError{Status: protocol.StatusBadRequest, Message: message}
}

var refusalStatuses = []string{
	protocol.StatusBadRequest, protocol.StatusNotFound, protocol.StatusUnauthorized,
	protocol.StatusNotPermitted, protocol.StatusRateLimited, protocol.StatusServerError,
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
