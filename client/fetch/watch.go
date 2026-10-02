package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/quic-go/quic-go"
)

// WatchRequest subscribes to change hints under Path: a prefix ending in "/"
// or one document. Since resumes after a cursor from an earlier watch.
// Coalesce asks for the newest event per path only when the watch lags.
type WatchRequest struct {
	Host, Path, Token string
	Since             protocol.Cursor
	Coalesce          bool
}

// Notice is one item from a watch: an event, or a resync telling the
// consumer to rebuild what it derived. Event.Cursor is where the watch
// continues from; on a resync the rest of Event is empty.
type Notice struct {
	Resync bool
	Event  protocol.WatchEvent
}

// StatusError is a server refusal that ends a watch: the status and body of
// the response given in place of the acknowledgement or as a terminal block.
type StatusError struct {
	Status  string
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return "watch refused: " + e.Status
	}
	return "watch refused: " + e.Status + ": " + e.Message
}

// watchQueueSize bounds notices waiting for the consumer. A full queue stops
// reading the stream, so a slow consumer never stalls the connection's other
// streams; the watch reopens from its cursor once there is room.
const watchQueueSize = 1024

// watchStall is how long a stream may stay silent before it is reopened:
// three missed heartbeats.
const watchStall = 3 * protocol.WatchHeartbeatInterval

// WatchStream is one WATCH stream after its request was sent: the blocks to
// read. A Client opens QUIC streams; NewWatch takes any other transport.
type WatchStream interface {
	io.Reader
	SetReadDeadline(time.Time) error
	// Abort unblocks a pending read at once; the stream is dead after it.
	Abort()
	// Release aborts the stream and hands it back, once; err is why it
	// ended, nil if cleanly.
	Release(err error)
}

// WatchDialer opens a stream to the watched world and sends req on it. ctx
// bounds opening and sending only; the stream lives until Release.
type WatchDialer func(ctx context.Context, req protocol.Request) (WatchStream, error)

// Watch is one subscription that outlives its streams: it reopens after a
// closing block, a lost connection or a stall, resuming from its cursor, and
// surfaces resync as a Notice.
type Watch struct {
	dial             WatchDialer
	handshakeTimeout time.Duration
	req              WatchRequest

	notices chan Notice
	done    chan struct{}
	cancel  context.CancelFunc

	mu sync.Mutex
	// stream is where the next stream reopens from; consumed is the cursor
	// of the last notice Next returned, which may lag behind what is queued.
	stream, consumed protocol.Cursor
	err              error
}

// Watch subscribes and returns once the server has acknowledged, or with the
// server's refusal. Events arrive through Next until ctx ends or the server
// ends the watch for good.
func (c *Client) Watch(ctx context.Context, r WatchRequest) (*Watch, error) {
	return NewWatch(ctx, c.watchDialer(r.Host), r, c.opts.RequestTimeout)
}

// NewWatch is Client.Watch over any transport: dial opens each stream, and
// handshakeTimeout bounds opening one through its acknowledgement.
func NewWatch(ctx context.Context, dial WatchDialer, r WatchRequest, handshakeTimeout time.Duration) (*Watch, error) {
	if r.Path == "" {
		return nil, errors.New("WATCH requires a path")
	}
	w := &Watch{dial: dial, handshakeTimeout: handshakeTimeout, req: r, notices: make(chan Notice, watchQueueSize), done: make(chan struct{}), stream: r.Since, consumed: r.Since}
	stream, err := w.open(ctx, r.Since)
	if err != nil {
		return nil, err
	}
	if len(w.notices) == 0 {
		// No resync to consume: the acknowledgement is where a caller resumes.
		w.consumed = w.stream
	}
	runCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go w.run(runCtx, stream)
	return w, nil
}

// Next returns the next notice, or the error that ended the watch.
func (w *Watch) Next(ctx context.Context) (Notice, error) {
	select {
	case n := <-w.notices:
		return w.consume(n), nil
	default:
	}
	select {
	case n := <-w.notices:
		return w.consume(n), nil
	case <-w.done:
		return Notice{}, w.Err()
	case <-ctx.Done():
		return Notice{}, ctx.Err()
	}
}

func (w *Watch) consume(n Notice) Notice {
	w.mu.Lock()
	w.consumed = n.Event.Cursor
	w.mu.Unlock()
	return n
}

// Cursor is where a later watch resumes without missing anything: the
// cursor of the last notice Next returned. Notices still queued are after
// it, so a caller that persists it and restarts sees them again.
func (w *Watch) Cursor() protocol.Cursor {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.consumed
}

// streamCursor is where the watch reopens its own stream: everything before
// it is queued or consumed.
func (w *Watch) streamCursor() protocol.Cursor {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stream
}

// Err is why the watch ended, once Next has reported it.
func (w *Watch) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Close ends the watch.
func (w *Watch) Close() {
	w.cancel()
	<-w.done
}

func (w *Watch) setCursor(c protocol.Cursor) {
	w.mu.Lock()
	w.stream = c
	w.mu.Unlock()
}

func (w *Watch) finish(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
	close(w.done)
}

// watchStream is one open WATCH stream and the reader over it.
type watchStream struct {
	stream WatchStream
	reader *protocol.WatchReader
	// ended is the read error that cut the stream, so a dead connection is
	// evicted on release; nil after a clean end.
	ended error
	// blocks counts what the stream carried after its acknowledgement.
	blocks int
}

func (s *watchStream) close() { s.stream.Release(s.ended) }

// quicWatchStream is a WATCH stream on a pooled connection.
type quicWatchStream struct {
	*quic.Stream
	client *Client
	host   string
	conn   *quic.Conn
}

func (s *quicWatchStream) Abort() { s.CancelRead(0) }

func (s *quicWatchStream) Release(err error) {
	s.CancelRead(0)
	s.client.dispose(s.host, s.conn, err)
}

// watchDialer opens WATCH streams on host's pooled connection.
func (c *Client) watchDialer(host string) WatchDialer {
	return func(ctx context.Context, req protocol.Request) (WatchStream, error) {
		conn, err := c.acquire(ctx, host)
		if err != nil {
			return nil, err
		}
		stream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			err = fmt.Errorf("open stream: %w", err)
			c.dispose(host, conn, err)
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() {
			stream.CancelRead(0)
			stream.CancelWrite(0)
		})
		err = sendRequest(ctx, stream, req)
		stop()
		if err != nil {
			c.dispose(host, conn, err)
			return nil, err
		}
		return &quicWatchStream{Stream: stream, client: c, host: host, conn: conn}, nil
	}
}

// ConnDialer is a WatchDialer over conns that dial opens with req sent, such
// as an in-process watch. The dial ctx bounds only the open, as net.Dialer's
// does: a conn tied to it would die with the handshake; the watch closes it.
func ConnDialer(dial func(ctx context.Context, req protocol.Request) (net.Conn, error)) WatchDialer {
	return func(ctx context.Context, req protocol.Request) (WatchStream, error) {
		conn, err := dial(context.WithoutCancel(ctx), req)
		if err != nil {
			return nil, err
		}
		return connStream{conn}, nil
	}
}

// connStream is a conn as a WatchStream: closing it unblocks a read.
type connStream struct{ net.Conn }

func (s connStream) Abort() {
	s.Close() //nolint:errcheck,gosec // the stream is dead either way; nothing reads why
}

func (s connStream) Release(error) { s.Abort() }

// errResyncFirst marks a resync given in place of the acknowledgement: the
// notice is queued and the watch subscribes again from the server's cursor.
var errResyncFirst = errors.New("resync in place of the acknowledgement")

// open subscribes on a pooled connection and reads the first block. A
// refusal is a StatusError; a resync first block is surfaced as a Notice and
// the subscription is retried from the cursor it carried.
func (w *Watch) open(ctx context.Context, since protocol.Cursor) (*watchStream, error) {
	for attempt := range maxRetries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		stream, err := w.subscribe(ctx, since)
		switch {
		case err == nil:
			return stream, nil
		case errors.Is(err, errResyncFirst):
			since = w.streamCursor()
		case attempt == maxRetries-1 || !isRetryable(err):
			return nil, err
		default:
			if err := waitForRetry(ctx, retryDelay); err != nil {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf("%w %d times", errResyncFirst, maxRetries)
}

// subscribe opens a stream from since and reads the first block within the
// handshake timeout. The reader that read it stays with the stream: its
// buffer may already hold the blocks that followed.
func (w *Watch) subscribe(ctx context.Context, since protocol.Cursor) (*watchStream, error) {
	handshake, cancel := context.WithTimeout(ctx, w.handshakeTimeout)
	defer cancel()
	meta := map[string]string{}
	if !since.IsZero() {
		meta["since"] = since.String()
	}
	if w.req.Coalesce {
		meta["coalesce"] = "path"
	}
	stream, err := w.dial(handshake, newRequest(protocol.VerbWatch, w.req.Path, w.req.Token, meta))
	if err != nil {
		return nil, err
	}
	// Stopped before the handshake context ends on success, so the stream
	// outlives it.
	stop := context.AfterFunc(handshake, stream.Abort)
	defer stop()
	ws := &watchStream{stream: stream, reader: protocol.NewWatchReader(stream)}
	first, err := ws.reader.Next()
	if err != nil {
		err = &sentError{cause: fmt.Errorf("read acknowledgement: %w", err)}
		stream.Release(err)
		return nil, err
	}
	if err := w.accept(ctx, first); err != nil {
		ws.close()
		return nil, err
	}
	return ws, nil
}

// accept applies the first block: ok continues, resync is surfaced and
// reported as errResyncFirst, anything else refuses the watch.
func (w *Watch) accept(ctx context.Context, first protocol.WatchBlock) error {
	if first.Status != protocol.StatusOK && first.Status != protocol.StatusResync {
		return &StatusError{Status: first.Status, Message: first.Metadata["message"]}
	}
	cursor, err := first.Cursor()
	if err != nil {
		return fmt.Errorf("%s: %w", first.Status, err)
	}
	w.setCursor(cursor)
	if first.Status == protocol.StatusOK {
		return nil
	}
	// The queue may be full on a reopen; a plain send would park run and
	// hang Close.
	select {
	case w.notices <- Notice{Resync: true, Event: protocol.WatchEvent{Cursor: cursor}}:
		return errResyncFirst
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run reads blocks until the watch ends, reopening the stream from the
// cursor whenever it can.
func (w *Watch) run(ctx context.Context, stream *watchStream) {
	defer w.cancel()
	backoff, empty := retryDelay, retryDelay
	for {
		delay, err := w.pump(ctx, stream)
		stream.close()
		if err != nil {
			w.finish(err)
			return
		}
		// A stream that ended carrying nothing reopens after a growing pause,
		// so a server ending every stream cannot spin the watch.
		if stream.blocks == 0 {
			delay = max(delay, empty)
			empty = min(empty*2, 5*time.Second)
		} else {
			empty = retryDelay
		}
		if err := waitForRetry(ctx, delay); err != nil {
			w.finish(err)
			return
		}
		for {
			var openErr error
			stream, openErr = w.open(ctx, w.streamCursor())
			if openErr == nil {
				backoff = retryDelay
				break
			}
			if ctx.Err() != nil || !reopenable(openErr) {
				w.finish(openErr)
				return
			}
			if err := waitForRetry(ctx, backoff); err != nil {
				w.finish(err)
				return
			}
			backoff = min(backoff*2, 5*time.Second)
		}
	}
}

// reopenable is whether a failed reopen is worth another try: a refusal is
// final unless it is a retry-later status (SPEC §7).
func reopenable(err error) bool {
	var refused *StatusError
	if !errors.As(err, &refused) {
		return true
	}
	return refused.Status == protocol.StatusRateLimited || refused.Status == protocol.StatusServerError
}

// pump delivers one stream's blocks until it ends. A nil error means the
// watch survives and reopens after delay: the server said closing, the
// stream was cut or stalled, or the queue wedged it.
func (w *Watch) pump(ctx context.Context, s *watchStream) (delay time.Duration, err error) {
	stop := context.AfterFunc(ctx, s.stream.Abort)
	defer stop()
	for {
		if err := s.stream.SetReadDeadline(time.Now().Add(watchStall)); err != nil {
			s.ended = err
			return 0, nil
		}
		block, err := s.reader.Next()
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if err != nil {
			// A cut stream, a stall or a malformed block: resume from the cursor.
			s.ended = err
			return 0, nil
		}
		s.blocks++
		if block.Status == "" {
			ev, err := block.Event()
			if err != nil {
				return 0, nil
			}
			w.setCursor(ev.Cursor)
			if protocol.IsKnownOp(ev.Op) && !w.deliver(ctx, s, &Notice{Event: ev}) {
				return 0, nil
			}
			continue
		}
		cursor, cursorErr := block.Cursor()
		if cursorErr == nil {
			w.setCursor(cursor)
		}
		switch block.Status {
		case protocol.StatusOK:
		case protocol.StatusResync:
			if cursorErr != nil {
				return 0, nil
			}
			w.deliver(ctx, s, &Notice{Resync: true, Event: protocol.WatchEvent{Cursor: cursor}})
			return 0, nil
		case protocol.StatusClosing:
			return 500 * time.Millisecond, nil
		default:
			return 0, &StatusError{Status: block.Status}
		}
	}
}

// deliver queues n. A full queue cancels the stream, so the connection's
// window stays free for other streams, then waits for room and reports false:
// the caller reopens after n's cursor, so nothing is lost.
func (w *Watch) deliver(ctx context.Context, s *watchStream, n *Notice) bool {
	select {
	case w.notices <- *n:
		return true
	default:
	}
	s.stream.Abort()
	select {
	case w.notices <- *n:
		return false
	case <-ctx.Done():
		return false
	}
}
