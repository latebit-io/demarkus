package fetch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/quic-go/quic-go"
)

// WatchRequest subscribes to change hints under Path: a prefix ending in "/"
// or one document. Since resumes after a cursor from an earlier watch.
type WatchRequest struct {
	Host, Path, Token string
	Since             protocol.Cursor
}

// Notice is one item from a watch: an event, or a resync telling the
// consumer to rebuild what it derived and continue from Cursor.
type Notice struct {
	Resync bool
	Event  protocol.WatchEvent // set unless Resync
	Cursor protocol.Cursor     // where the watch continues from
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

// Watch is one subscription that outlives its streams: it reopens after a
// closing block, a lost connection or a stall, resuming from its cursor, and
// surfaces resync as a Notice.
type Watch struct {
	client *Client
	req    WatchRequest

	notices chan Notice
	done    chan struct{}
	cancel  context.CancelFunc

	mu     sync.Mutex
	cursor protocol.Cursor
	err    error
}

// Watch subscribes and returns once the server has acknowledged, or with the
// server's refusal. Events arrive through Next until ctx ends or the server
// ends the watch for good.
func (c *Client) Watch(ctx context.Context, r WatchRequest) (*Watch, error) {
	if r.Path == "" {
		return nil, errors.New("WATCH requires a path")
	}
	w := &Watch{client: c, req: r, notices: make(chan Notice, watchQueueSize), done: make(chan struct{}), cursor: r.Since}
	stream, err := w.open(ctx, r.Since)
	if err != nil {
		return nil, err
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
		return n, nil
	default:
	}
	select {
	case n := <-w.notices:
		return n, nil
	case <-w.done:
		return Notice{}, w.Err()
	case <-ctx.Done():
		return Notice{}, ctx.Err()
	}
}

// Cursor is the position the watch resumes from.
func (w *Watch) Cursor() protocol.Cursor {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cursor
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
	w.cursor = c
	w.mu.Unlock()
}

func (w *Watch) finish(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
	close(w.done)
}

// watchStream is one open WATCH stream on a pooled connection.
type watchStream struct {
	conn   *quic.Conn
	stream *quic.Stream
	reader *protocol.WatchReader
}

func (s *watchStream) close(c *Client) {
	s.stream.CancelRead(0)
	c.release(s.conn)
}

// errResyncFirst marks a resync given in place of the acknowledgement: the
// notice is queued and the watch subscribes again from the server's cursor.
var errResyncFirst = errors.New("resync in place of the acknowledgement")

// open subscribes on a pooled connection and reads the first block. A
// refusal is a StatusError; a resync first block is surfaced as a Notice and
// the subscription is retried from the cursor it carried.
func (w *Watch) open(ctx context.Context, since protocol.Cursor) (*watchStream, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := range maxAttempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		stream, err := w.subscribe(ctx, since)
		if err == nil {
			return stream, nil
		}
		if errors.Is(err, errResyncFirst) {
			since = w.Cursor()
			continue
		}
		lastErr = err
		if attempt == maxAttempts-1 || !isRetryable(err) {
			return nil, err
		}
		if err := waitForRetry(ctx, 100*time.Millisecond); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (w *Watch) subscribe(ctx context.Context, since protocol.Cursor) (*watchStream, error) {
	c := w.client
	conn, err := c.acquire(ctx, w.req.Host)
	if err != nil {
		return nil, err
	}
	ws, first, err := w.subscribeOn(ctx, conn, since)
	if err != nil {
		if isConnectionError(err) || conn.Context().Err() != nil {
			c.evict(w.req.Host, conn)
		}
		c.release(conn)
		return nil, err
	}
	if err := w.accept(first); err != nil {
		ws.close(c)
		return nil, err
	}
	return ws, nil
}

// subscribeOn sends the request and reads the first block within the
// request timeout. The reader that read it stays with the stream: its buffer
// may already hold the blocks that followed.
func (w *Watch) subscribeOn(ctx context.Context, conn *quic.Conn, since protocol.Cursor) (*watchStream, protocol.WatchBlock, error) {
	handshake, cancel := context.WithTimeout(ctx, w.client.opts.RequestTimeout)
	defer cancel()
	stream, err := conn.OpenStreamSync(handshake)
	if err != nil {
		return nil, protocol.WatchBlock{}, fmt.Errorf("open stream: %w", err)
	}
	meta := map[string]string{}
	if !since.IsZero() {
		meta["since"] = since.String()
	}
	req := newRequest(protocol.VerbWatch, w.req.Path, w.req.Token, meta)
	if _, err := req.WriteTo(stream); err != nil {
		stream.CancelRead(0)
		stream.CancelWrite(0)
		return nil, protocol.WatchBlock{}, fmt.Errorf("send request: %w", err)
	}
	if err := stream.Close(); err != nil {
		stream.CancelRead(0)
		return nil, protocol.WatchBlock{}, &sentError{cause: fmt.Errorf("close request stream: %w", err)}
	}
	deadline, _ := handshake.Deadline()
	if err := stream.SetReadDeadline(deadline); err != nil {
		stream.CancelRead(0)
		return nil, protocol.WatchBlock{}, fmt.Errorf("set read deadline: %w", err)
	}
	ws := &watchStream{conn: conn, stream: stream, reader: protocol.NewWatchReader(stream)}
	first, err := ws.reader.Next()
	if err != nil {
		stream.CancelRead(0)
		return nil, protocol.WatchBlock{}, &sentError{cause: fmt.Errorf("read acknowledgement: %w", err)}
	}
	return ws, first, nil
}

// accept applies the first block: ok continues, resync is surfaced and
// reported as errResyncFirst, anything else refuses the watch.
func (w *Watch) accept(first protocol.WatchBlock) error {
	switch first.Status {
	case protocol.StatusOK:
		cursor, err := first.Cursor()
		if err != nil {
			return fmt.Errorf("acknowledgement: %w", err)
		}
		w.setCursor(cursor)
		return nil
	case protocol.StatusResync:
		cursor, err := first.Cursor()
		if err != nil {
			return fmt.Errorf("resync: %w", err)
		}
		w.setCursor(cursor)
		w.notices <- Notice{Resync: true, Cursor: cursor}
		return errResyncFirst
	default:
		return &StatusError{Status: first.Status, Message: first.Metadata["message"]}
	}
}

// run reads blocks until the watch ends, reopening the stream from the
// cursor whenever it can.
func (w *Watch) run(ctx context.Context, stream *watchStream) {
	defer w.cancel()
	backoff := 100 * time.Millisecond
	for {
		reopen, err := w.pump(ctx, stream)
		stream.close(w.client)
		if err != nil {
			w.finish(err)
			return
		}
		if reopen != nil {
			// Not a fault: the server said closing, or the stream was
			// wedged by a queue we could not fill. Reopen after a pause.
			if err := waitForRetry(ctx, reopen()); err != nil {
				w.finish(err)
				return
			}
		}
		for {
			var openErr error
			stream, openErr = w.open(ctx, w.Cursor())
			if openErr == nil {
				backoff = 100 * time.Millisecond
				break
			}
			var refused *StatusError
			if errors.As(openErr, &refused) || ctx.Err() != nil {
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

// pump delivers one stream's blocks. It returns a non nil reopen delay when
// the stream ended in a way the watch survives, an error when it does not.
func (w *Watch) pump(ctx context.Context, s *watchStream) (reopen func() time.Duration, err error) {
	stop := context.AfterFunc(ctx, func() { s.stream.CancelRead(0) })
	defer stop()
	instant := func() time.Duration { return 0 }
	for {
		if err := s.stream.SetReadDeadline(time.Now().Add(watchStall)); err != nil {
			return instant, nil
		}
		block, err := s.reader.Next()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			// A cut stream, a stall or a malformed block: resume from the cursor.
			return instant, nil
		}
		if block.Status == "" {
			ev, err := block.Event()
			if err != nil {
				return instant, nil
			}
			w.setCursor(ev.Cursor)
			if !protocol.IsKnownOp(ev.Op) {
				continue
			}
			if !w.deliver(ctx, s, &Notice{Event: ev, Cursor: ev.Cursor}) {
				return instant, nil
			}
			continue
		}
		cursor, cursorErr := block.Cursor()
		switch block.Status {
		case protocol.StatusOK:
			if cursorErr == nil {
				w.setCursor(cursor)
			}
		case protocol.StatusResync:
			if cursorErr != nil {
				return instant, nil
			}
			w.setCursor(cursor)
			w.deliver(ctx, s, &Notice{Resync: true, Cursor: cursor})
			return instant, nil
		case protocol.StatusClosing:
			if cursorErr == nil {
				w.setCursor(cursor)
			}
			return func() time.Duration { return 500 * time.Millisecond }, nil
		default:
			return nil, &StatusError{Status: block.Status}
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
	s.stream.CancelRead(0)
	select {
	case w.notices <- *n:
		return false
	case <-ctx.Done():
		return false
	}
}
