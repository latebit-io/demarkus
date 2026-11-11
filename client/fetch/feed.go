package fetch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// Feed is the half of a watch every transport shares: the bounded notice
// queue, the resume and consumer cursors, and how the watch ends. A transport
// offers what its streams carry; the feed decides what is new, owed or full.
type Feed struct {
	notices chan Notice
	room    chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc

	mu sync.Mutex
	// stream is where the next stream resumes; consumed is the cursor of the
	// last notice Next returned, which may lag behind what is queued.
	stream, consumed protocol.Cursor
	// owed is a resync not yet queued; it is paid before any later event, or
	// the consumer's cursor would pass the gap it reports.
	owed   bool
	queued uint64
	err    error
}

// NewFeed is the feed of one watch resuming after since. queue bounds the
// notices held for the consumer; 0 holds 1024.
func NewFeed(since protocol.Cursor, queue int) (*Feed, error) {
	if queue < 0 {
		return nil, fmt.Errorf("WATCH queue %d is negative", queue)
	}
	if queue == 0 {
		queue = watchQueueSize
	}
	return &Feed{
		notices: make(chan Notice, queue), room: make(chan struct{}, 1), done: make(chan struct{}),
		stream: since, consumed: since,
	}, nil
}

// Start hands the feed to the goroutine following its streams: cancel stops
// that goroutine, which calls Finish once on its way out.
func (f *Feed) Start(cancel context.CancelFunc) { f.cancel = cancel }

// Next returns the next notice, or the error that ended the watch.
func (f *Feed) Next(ctx context.Context) (Notice, error) {
	select {
	case n := <-f.notices:
		return f.consume(n), nil
	default:
	}
	select {
	case n := <-f.notices:
		return f.consume(n), nil
	case <-f.done:
		return Notice{}, f.Err()
	case <-ctx.Done():
		return Notice{}, ctx.Err()
	}
}

func (f *Feed) consume(n Notice) Notice {
	f.mu.Lock()
	f.consumed = n.Event.Cursor
	f.mu.Unlock()
	select {
	case f.room <- struct{}{}:
	default:
	}
	return n
}

// Cursor is where a later watch resumes without missing anything: the
// cursor of the last notice Next returned. Notices still queued are after
// it, so a caller that persists it and restarts sees them again.
func (f *Feed) Cursor() protocol.Cursor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.consumed
}

// StreamCursor is where the next stream resumes: everything before it is
// queued or consumed.
func (f *Feed) StreamCursor() protocol.Cursor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stream
}

// Err is why the watch ended, once Next has reported it.
func (f *Feed) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// Close ends the watch and waits for its follower to finish.
func (f *Feed) Close() {
	f.cancel()
	<-f.done
}

// Finish ends the watch with err; the follower calls it once.
func (f *Feed) Finish(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
	f.cancel()
	close(f.done)
}

// Queued counts the notices queued so far, so a follower can tell a stream
// that delivered from one that did not.
func (f *Feed) Queued() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queued
}

// Skip moves the stream cursor past what yields no notice: an
// acknowledgement or a heartbeat.
func (f *Feed) Skip(cursor protocol.Cursor) {
	f.mu.Lock()
	f.stream = cursor
	f.mu.Unlock()
}

// settle makes the stream cursor the consumer's when nothing waits to be
// consumed: an acknowledgement is then where a caller resumes.
func (f *Feed) settle() {
	f.mu.Lock()
	if len(f.notices) == 0 && !f.owed {
		f.consumed = f.stream
	}
	f.mu.Unlock()
}

// Event takes one event without blocking. False means the queue is full and
// the event was not taken: the stream stops, and a later one resumes from
// StreamCursor after WaitRoom. A repeat is dropped; an unknown op is passed.
func (f *Feed) Event(ev *protocol.WatchEvent) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !after(ev.Cursor, f.stream) {
		return true
	}
	if !protocol.IsKnownOp(ev.Op) {
		f.stream = ev.Cursor
		return true
	}
	if !f.payLocked() {
		return false
	}
	select {
	case f.notices <- Notice{Event: *ev}:
		f.stream = ev.Cursor
		f.queued++
		return true
	default:
		return false
	}
}

// Resync tells the consumer to rebuild what it derived and continue after
// cursor, because the server said so. A full queue keeps it owed and reports
// false, as Event does.
func (f *Feed) Resync(cursor protocol.Cursor) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stream, f.owed = cursor, true
	return f.payLocked()
}

// Unread passes an event that could not be decoded but names its cursor: the
// consumer missed it, so a resync is owed. A repeat is dropped as Event drops
// one, so a server resending the block cannot move the cursor back.
func (f *Feed) Unread(cursor protocol.Cursor) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !after(cursor, f.stream) {
		return true
	}
	f.stream, f.owed = cursor, true
	return f.payLocked()
}

// Owe records a resync for a gap no stream reports: a reopen from no cursor
// starts at the head and never sends what landed meanwhile.
func (f *Feed) Owe() {
	f.mu.Lock()
	f.owed = true
	f.mu.Unlock()
}

// Pay queues an owed resync on a stream that has sent nothing yet; false
// when the queue is full, as Event reports it.
func (f *Feed) Pay() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.payLocked()
}

// payLocked queues the owed resync at the stream cursor, so Cursor never
// moves back. Callers hold f.mu.
func (f *Feed) payLocked() bool {
	if !f.owed {
		return true
	}
	select {
	case f.notices <- Notice{Resync: true, Event: protocol.WatchEvent{Cursor: f.stream}}:
		f.owed = false
		f.queued++
		return true
	default:
		return false
	}
}

// WaitRoom blocks until the consumer has made room in a full queue.
func (f *Feed) WaitRoom(ctx context.Context) error {
	for len(f.notices) == cap(f.notices) {
		select {
		case <-f.room:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// after reports whether cursor is past last within one epoch; another epoch
// is always new.
func after(cursor, last protocol.Cursor) bool {
	return last.IsZero() || cursor.Epoch != last.Epoch || cursor.Seq > last.Seq
}

// RetryLater reports a failed open worth another try: a refusal is final
// unless it is a retry-later status (SPEC §7); anything else is transport.
func RetryLater(err error) bool {
	refused, ok := errors.AsType[*StatusError](err)
	if !ok {
		return true
	}
	return refused.Status == protocol.StatusRateLimited || refused.Status == protocol.StatusServerError
}

// maxPause caps the pauses a Pace grows; maxUnreadable is how many streams
// in a row may end on a block the client cannot place before the watch ends.
const (
	maxPause      = 5 * time.Second
	maxUnreadable = 5
)

// Pace is when a watch reopens, shared by every transport. A stream without
// progress reopens after a growing pause, so a server ending every stream or
// resending an unreadable block cannot spin the watch.
type Pace struct {
	idle, failed time.Duration
	unreadable   int
}

// AfterStream is the pause before reopening once a stream ended. progress
// says it queued a notice or ended in order; floor is the least to wait.
func (p *Pace) AfterStream(progress bool, floor time.Duration) time.Duration {
	if progress {
		p.idle, p.unreadable = 0, 0
		return floor
	}
	p.idle = grow(p.idle)
	return max(floor, p.idle)
}

// AfterFailedOpen is the pause before the next try at opening a stream.
func (p *Pace) AfterFailedOpen() time.Duration {
	p.failed = grow(p.failed)
	return p.failed
}

// Opened resets the failed-open backoff.
func (p *Pace) Opened() { p.failed = 0 }

// Unreadable counts a stream that ended on a block the client cannot read
// or place. It returns an error once that many in a row made no progress:
// the same block is being resent, and the watch cannot pass it.
func (p *Pace) Unreadable(cause error) error {
	p.unreadable++
	if p.unreadable < maxUnreadable {
		return nil
	}
	return fmt.Errorf("watch cannot pass a block after %d tries: %w", p.unreadable, cause)
}

func grow(pause time.Duration) time.Duration {
	if pause == 0 {
		return retryDelay
	}
	return min(pause*2, maxPause)
}
