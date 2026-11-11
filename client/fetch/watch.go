package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/quic-go/quic-go"
)

// WatchRequest subscribes to change hints under Path, a "/" prefix or one
// document. Since resumes after a cursor; Coalesce asks for the newest event
// per path when lagging; Queue bounds notices held for the consumer (0: 1024).
type WatchRequest struct {
	Host, Path, Token string
	Since             protocol.Cursor
	Coalesce          bool
	Queue             int
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
// surfaces resync as a Notice. Next, Cursor, Err and Close are its Feed's.
type Watch struct {
	*Feed
	dial             WatchDialer
	handshakeTimeout time.Duration
	req              WatchRequest
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
	feed, err := NewFeed(r.Since, r.Queue)
	if err != nil {
		return nil, err
	}
	w := &Watch{Feed: feed, dial: dial, handshakeTimeout: handshakeTimeout, req: r}
	stream, err := w.open(ctx, r.Since)
	if err != nil {
		return nil, err
	}
	w.settle()
	runCtx, cancel := context.WithCancel(ctx)
	w.Start(cancel)
	go w.run(runCtx, stream)
	return w, nil
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
	// unreadable is a block that named no cursor to continue from; the
	// stream ended on it.
	unreadable error
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
// as an in-process watch. As with net.Dialer, dial's ctx bounds only the
// open: the conn must outlive it, and the watch closes it.
func ConnDialer(dial func(ctx context.Context, req protocol.Request) (net.Conn, error)) WatchDialer {
	return func(ctx context.Context, req protocol.Request) (WatchStream, error) {
		conn, err := dial(ctx, req)
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
			since = w.StreamCursor()
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
	if err := w.accept(first); err != nil {
		ws.close()
		return nil, err
	}
	return ws, nil
}

// accept applies the first block: ok continues, resync is surfaced and
// reported as errResyncFirst, anything else refuses the watch.
func (w *Watch) accept(first protocol.WatchBlock) error {
	if first.Status != protocol.StatusOK && first.Status != protocol.StatusResync {
		return &StatusError{Status: first.Status, Message: first.Metadata["message"]}
	}
	cursor, err := first.Cursor()
	if err != nil {
		return fmt.Errorf("%s: %w", first.Status, err)
	}
	if first.Status == protocol.StatusOK {
		w.Skip(cursor)
		return nil
	}
	// A full queue keeps the resync owed; the next stream pays it.
	w.Resync(cursor)
	return errResyncFirst
}

// run follows streams until the watch ends, reopening from the cursor
// whenever it can, at the feed's pace.
func (w *Watch) run(ctx context.Context, stream *watchStream) {
	var pace Pace
	for {
		before := w.Queued()
		floor, err := w.pump(ctx, stream)
		stream.close()
		queued := w.Queued() > before
		if err == nil && stream.unreadable != nil && !queued {
			err = pace.Unreadable(stream.unreadable)
		}
		if err != nil {
			w.Finish(err)
			return
		}
		// A stream that ended in order after carrying blocks is progress too:
		// a quiet world's streams are closed, not spun.
		progress := queued || (stream.blocks > 0 && stream.ended == nil && stream.unreadable == nil)
		if err := waitForRetry(ctx, pace.AfterStream(progress, floor)); err != nil {
			w.Finish(err)
			return
		}
		if stream, err = w.reopen(ctx, &pace); err != nil {
			w.Finish(err)
			return
		}
	}
}

// reopen opens the next stream from the cursor, retrying what RetryLater
// says is worth it.
func (w *Watch) reopen(ctx context.Context, pace *Pace) (*watchStream, error) {
	for {
		stream, err := w.open(ctx, w.StreamCursor())
		if err == nil {
			pace.Opened()
			return stream, nil
		}
		if ctx.Err() != nil || !RetryLater(err) {
			return nil, err
		}
		if err := waitForRetry(ctx, pace.AfterFailedOpen()); err != nil {
			return nil, err
		}
	}
}

// pump takes one stream's blocks until it ends. A nil error means the watch
// survives and reopens after floor: the server said closing, the stream was
// cut or stalled, a block named no cursor, or the queue filled.
func (w *Watch) pump(ctx context.Context, s *watchStream) (floor time.Duration, err error) {
	stop := context.AfterFunc(ctx, s.stream.Abort)
	defer stop()
	// Close aborted the stream under the loop: a clean end, whatever the
	// deadline or read reported.
	defer func() {
		if err == nil && ctx.Err() != nil {
			s.ended, err = nil, ctx.Err()
		}
	}()
	// A quiet stream still pays a resync owed from before it opened.
	if !w.Pay() {
		return 0, w.overflow(ctx, s)
	}
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
			// A cut stream, a stall or broken framing: resume from the cursor.
			s.ended = err
			return 0, nil
		}
		s.blocks++
		if block.Status == "" {
			if w.event(s, block) {
				continue
			}
			if s.unreadable != nil {
				return 0, nil
			}
			return 0, w.overflow(ctx, s)
		}
		if done, floor, err := w.control(ctx, s, block); done {
			return floor, err
		}
	}
}

// event takes one event block; false stops the stream, on a full queue or
// with s.unreadable set. A block that reads as no event but names a cursor
// is passed and reported as a resync: the consumer missed what it announced.
func (w *Watch) event(s *watchStream, block protocol.WatchBlock) bool {
	ev, err := block.Event()
	if err == nil {
		return w.Event(&ev)
	}
	cursor, cursorErr := block.Cursor()
	if cursorErr != nil {
		s.unreadable = err
		return false
	}
	return w.Unread(cursor)
}

// control applies a status block; done ends the stream as pump reports it.
func (w *Watch) control(ctx context.Context, s *watchStream, block protocol.WatchBlock) (done bool, floor time.Duration, err error) {
	cursor, cursorErr := block.Cursor()
	if block.Status == protocol.StatusResync {
		if cursorErr != nil {
			s.unreadable = cursorErr
			return true, 0, nil
		}
		if !w.Resync(cursor) {
			return true, 0, w.overflow(ctx, s)
		}
		return true, 0, nil
	}
	if cursorErr == nil {
		w.Skip(cursor)
	}
	switch block.Status {
	case protocol.StatusOK:
		return false, 0, nil
	case protocol.StatusClosing:
		return true, 500 * time.Millisecond, nil
	default:
		return true, 0, &StatusError{Status: block.Status}
	}
}

// overflow cancels the stream over a full queue, so the connection's window
// stays free for other streams, and waits for room: the next stream resumes
// from the cursor, so nothing is lost.
func (w *Watch) overflow(ctx context.Context, s *watchStream) error {
	s.stream.Abort()
	return w.WaitRoom(ctx)
}
