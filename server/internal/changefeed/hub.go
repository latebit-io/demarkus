// Package changefeed is a world's change hub: committed writes append hints
// to a ring, watchers read at their own pace, and a watcher the ring has
// lapped is told to resync. It depends on no store; the write path never
// blocks on a reader.
package changefeed

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/latebit-io/demarkus/protocol"
)

// DefaultRingSize is how many events a hub retains for resume.
const DefaultRingSize = 4096

// ErrResync means the hub cannot continue from a cursor: another epoch, a
// position it never issued, or events since it already overwritten.
var ErrResync = errors.New("changefeed: cursor cannot be resumed")

// ErrClosed means the hub was closed under a subscriber.
var ErrClosed = errors.New("changefeed: hub closed")

// Event is one committed change under the store's sequence.
type Event struct {
	Seq     uint64
	Path    string
	Version int
	Hash    string
	Op      string
	Agent   string
}

// Hub holds a world's recent events and wakes subscribers on each publish.
// Seq doubles as the ring position, so the ring holds the last len(buf) seqs;
// a seq nobody published leaves a stale slot that readers skip.
type Hub struct {
	epoch string

	mu   sync.Mutex
	size uint64 // events the ring holds
	// buf is the ring; a hub with a backlog drops it while no subscription
	// is open, and resumes page from the backlog until it is back.
	buf         []Event
	subscribers int
	backlog     Backlog
	lastSeq     uint64
	floor       uint64 // seqs at or below it cannot be resumed from: see Skip
	notify      chan struct{}
	closed      bool
	// catching is the catch-up in flight; resumes that arrive meanwhile
	// share it rather than each polling the store.
	catching *catchUpFlight
	// waiting counts readers blocked in Next.
	waiting atomic.Int64
}

type catchUpFlight struct {
	done chan struct{}
	err  error
}

// Backlog is a store's durable record of events, shared by every replica
// that opens the store, so this hub can lag what peers committed.
type Backlog interface {
	// Reaches reports whether a resume from after may read the backlog while
	// the head is at head: nil, or why it resyncs. It bounds what one resume
	// costs the store.
	Reaches(ctx context.Context, after, head uint64) error
	// Events returns every event with after < Seq <= through, in order, in a
	// slice the caller keeps, or an error when it cannot name them all.
	Events(ctx context.Context, after, through uint64) ([]Event, error)
	// CatchUp publishes to the hub what peer replicas committed since this
	// replica last looked. It bounds its own wait: callers may detach ctx.
	CatchUp(ctx context.Context) error
}

// New makes a hub. An empty epoch gets a random one, which is right for a
// store whose sequence restarts with the process. ringSize <= 0 takes the
// default.
func New(epoch string, ringSize int) *Hub {
	return NewWithBacklog(epoch, ringSize, nil)
}

// NewWithBacklog is New for a store that keeps events past the ring: a
// resume older than the ring, as far back as the backlog Reaches, reads it a
// ring's worth at a time until it reaches the ring. Nil is New.
func NewWithBacklog(epoch string, ringSize int, backlog Backlog) *Hub {
	if epoch == "" {
		epoch = NewEpoch()
	}
	if ringSize <= 0 {
		ringSize = DefaultRingSize
	}
	h := &Hub{epoch: epoch, backlog: backlog, notify: make(chan struct{})}
	if ringSize > 0 {
		h.size = uint64(ringSize)
	}
	if backlog == nil {
		h.buf = make([]Event, h.size)
	}
	return h
}

// NewEpoch returns a random epoch in the cursor grammar.
func NewEpoch() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; a panic is the
		// honest report if it ever does.
		panic("changefeed: random epoch: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Epoch is the cursor epoch every event of this hub carries.
func (h *Hub) Epoch() string { return h.epoch }

// Waiting is how many readers are blocked in Next for the next event, so a
// store can skip work only a live watcher would see.
func (h *Hub) Waiting() int { return int(h.waiting.Load()) }

// RingSize is how many events the hub retains for resume.
func (h *Hub) RingSize() int { return int(h.size) } //nolint:gosec // set from a positive int

// Head is the cursor of the newest event, or seq 0 before any.
func (h *Hub) Head() protocol.Cursor {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cursor(h.lastSeq)
}

func (h *Hub) cursor(seq uint64) protocol.Cursor {
	return protocol.Cursor{Epoch: h.epoch, Seq: seq}
}

// Retained copies the events the ring holds, oldest first.
func (h *Hub) Retained() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.buf == nil {
		return nil
	}
	events := make([]Event, 0, h.lastSeq+1-h.oldest())
	for seq := h.oldest(); seq <= h.lastSeq; seq++ {
		if ev := h.buf[h.slot(seq)]; ev.Seq == seq {
			events = append(events, ev)
		}
	}
	return events
}

// PublishAt appends ev under the store's own Seq (backend.ChangeSource); it
// never blocks on a reader. A Seq already passed is dropped (false); a Seq
// past the next leaves a gap nobody can name, unresumable as after Skip.
func (h *Hub) PublishAt(ev Event) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ev.Seq <= h.lastSeq {
		return false
	}
	if ev.Seq > h.lastSeq+1 {
		h.floor = max(h.floor, ev.Seq-1)
	}
	h.append(ev)
	return true
}

// Skip moves the head to seq without events for what lies between: a
// subscriber behind it, and a cursor before it, get resync.
func (h *Hub) Skip(seq uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if seq <= h.floor {
		return
	}
	h.floor = seq
	if seq > h.lastSeq {
		h.lastSeq = seq
	}
	h.wake()
}

func (h *Hub) append(ev Event) {
	if h.buf == nil {
		// Nobody reads the ring: the backlog serves this event to a resume.
		h.floor = ev.Seq
	} else {
		h.buf[h.slot(ev.Seq)] = ev
	}
	h.lastSeq = ev.Seq
	h.wake()
}

// retainLocked counts a subscription in, bringing the ring back.
func (h *Hub) retainLocked() {
	h.subscribers++
	if h.buf == nil {
		h.buf, h.floor = make([]Event, h.size), max(h.floor, h.lastSeq)
	}
}

// release counts a subscription out; the last one out drops a backlogged
// hub's ring, so a world nobody watches holds none.
func (h *Hub) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subscribers--
	if h.subscribers == 0 && h.backlog != nil {
		h.buf, h.floor = nil, max(h.floor, h.lastSeq)
	}
}

func (h *Hub) wake() {
	close(h.notify)
	h.notify = make(chan struct{})
}

func (h *Hub) slot(seq uint64) uint64 { return seq % h.size }

// oldest is the Seq of the oldest event still in the ring, or the first
// after a skip.
func (h *Hub) oldest() uint64 {
	oldest := uint64(1)
	if n := h.size; h.lastSeq > n {
		oldest = h.lastSeq - n + 1
	}
	return max(oldest, h.floor+1)
}

// Close ends every subscriber with ErrClosed. Later publishes still append,
// so a write committing during a drain is not lost from the ring.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	close(h.notify)
	h.notify = make(chan struct{})
}

// Subscription reads one scope's events from the hub in order: from the
// backlog while it is older than the ring, then the ring.
type Subscription struct {
	hub     *Hub
	scope   string
	paging  bool    // next is still read from the backlog
	backlog []Event // unread in-scope events of the page read last, in order
	next    uint64  // Seq of the next event to read, from a page or the ring
	closed  bool
}

// Close ends the subscription; later reads return ErrClosed. Idempotent, and
// a no-op on nil.
func (s *Subscription) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed, s.backlog = true, nil
	s.hub.release()
}

// Subscribe starts a subscription over scope ("/", a "/"-ended subtree, or
// one document) at the head, after since, or ErrResync; close it when done,
// as it holds the ring. A resume before the ring reads the backlog.
func (h *Hub) Subscribe(ctx context.Context, scope string, since protocol.Cursor) (*Subscription, error) {
	if err := h.catchUp(ctx, since); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.retainLocked()
	head, oldest := h.lastSeq, h.oldest()
	h.mu.Unlock()
	s, err := h.open(ctx, scope, since, ringPosition{head: head, oldest: oldest})
	if err != nil {
		h.release()
		return nil, err
	}
	return s, nil
}

// ringPosition is the hub's head and oldest resumable seq, read together.
type ringPosition struct{ head, oldest uint64 }

// open is Subscribe's subscription once the ring is retained for it.
func (h *Hub) open(ctx context.Context, scope string, since protocol.Cursor, at ringPosition) (*Subscription, error) {
	head, oldest := at.head, at.oldest
	switch {
	case since.IsZero():
		return &Subscription{hub: h, scope: scope, next: head + 1}, nil
	case since.Epoch != h.epoch || since.Seq > head:
		return nil, ErrResync
	case since.Seq+1 >= oldest:
		// Next resyncs if the ring passes since before the first read.
		return &Subscription{hub: h, scope: scope, next: since.Seq + 1}, nil
	case h.backlog == nil:
		return nil, ErrResync
	}
	if err := h.backlog.Reaches(ctx, since.Seq, head); err != nil {
		return nil, fmt.Errorf("%w: backlog: %w", ErrResync, err)
	}
	// The first page is read now, so a backlog that cannot serve since
	// resyncs the subscribe rather than its first read.
	s := &Subscription{hub: h, scope: scope, paging: true, next: since.Seq + 1}
	if err := s.page(ctx, oldest-1); err != nil {
		return nil, err
	}
	return s, nil
}

// catchUp asks the store once for peer commits when since is past this
// hub's head: a resume that moved replicas is not a gap.
func (h *Hub) catchUp(ctx context.Context, since protocol.Cursor) error {
	if h.backlog == nil || since.IsZero() || since.Epoch != h.epoch {
		return nil
	}
	ran := false
	for {
		h.mu.Lock()
		if since.Seq <= h.lastSeq {
			h.mu.Unlock()
			return nil
		}
		flight := h.catching
		if flight == nil {
			// One that started before since was committed may have missed it,
			// so a waiter still behind starts its own, once.
			if ran {
				h.mu.Unlock()
				return nil
			}
			flight = &catchUpFlight{done: make(chan struct{})}
			h.catching = flight
			// Detached, so the caller that started it leaving fails no waiter.
			go h.fly(context.WithoutCancel(ctx), flight)
			ran = true
		}
		h.mu.Unlock()
		select {
		case <-flight.done:
		case <-ctx.Done():
			return fmt.Errorf("%w: catch up: %w", ErrResync, ctx.Err())
		}
		if flight.err != nil {
			return fmt.Errorf("%w: catch up: %w", ErrResync, flight.err)
		}
	}
}

// fly runs one shared catch-up and lets its waiters go.
func (h *Hub) fly(ctx context.Context, flight *catchUpFlight) {
	flight.err = h.backlog.CatchUp(ctx)
	h.mu.Lock()
	h.catching = nil
	h.mu.Unlock()
	close(flight.done)
}

// page reads the backlog after next, at most a ring's worth and none past
// through, outside the hub lock, and keeps what is in scope.
func (s *Subscription) page(ctx context.Context, through uint64) error {
	h := s.hub
	after := s.next - 1
	through = min(through, after+h.size)
	events, err := h.backlog.Events(ctx, after, through)
	if err != nil {
		return fmt.Errorf("%w: backlog: %w", ErrResync, err)
	}
	want := after
	for _, ev := range events {
		want++
		if ev.Seq != want {
			return fmt.Errorf("%w: backlog event has seq %d, want %d", ErrResync, ev.Seq, want)
		}
	}
	if want != through {
		return fmt.Errorf("%w: backlog ended at seq %d, want %d", ErrResync, want, through)
	}
	// A narrow scope keeps a fresh slice, so an idle watch holds only its own
	// events; the backlog's slice is the subscription's to keep.
	kept := events
	if s.scope != "/" {
		kept = nil
		for _, ev := range events {
			if inScope(s.scope, ev.Path) {
				kept = append(kept, ev)
			}
		}
	}
	s.backlog, s.next = kept, through+1
	return nil
}

// Scope is what the subscription was opened over.
func (s *Subscription) Scope() string { return s.scope }

// Cursor is the position to resume from: just before the next event in
// scope, so a resume replays as little as possible.
func (s *Subscription) Cursor() protocol.Cursor {
	if len(s.backlog) > 0 {
		return s.hub.cursor(s.backlog[0].Seq - 1)
	}
	return s.hub.cursor(s.next - 1)
}

// Next returns the next event in scope, waiting for one. It returns
// ErrResync once the ring has overwritten unread events or the backlog
// cannot name them, ErrClosed after Close, or ctx's error.
func (s *Subscription) Next(ctx context.Context) (Event, error) {
	for {
		ev, wait, err := s.pending(ctx)
		if err != nil || wait == nil {
			return ev, err
		}
		if err := s.hub.await(ctx, wait); err != nil {
			return Event{}, err
		}
	}
}

// await blocks until wait closes or ctx ends, counted as a waiting reader.
func (h *Hub) await(ctx context.Context, wait <-chan struct{}) error {
	h.waiting.Add(1)
	defer h.waiting.Add(-1)
	select {
	case <-wait:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryNext is Next without waiting for a publish: ErrIdle when nothing in
// scope is published yet. It may read the backlog under ctx.
func (s *Subscription) TryNext(ctx context.Context) (Event, error) {
	ev, wait, err := s.pending(ctx)
	if err == nil && wait != nil {
		return Event{}, ErrIdle
	}
	return ev, err
}

// ErrIdle is TryNext's answer when the subscription is at the head.
var ErrIdle = errors.New("changefeed: no event pending")

// pending returns the next event in scope, or the channel a publish closes
// when there is none yet. A paging subscription reads pages until it reaches
// the ring, which may move on meanwhile, so a slow reader is not lapped.
func (s *Subscription) pending(ctx context.Context) (Event, <-chan struct{}, error) {
	h := s.hub
	if s.closed {
		return Event{}, nil, ErrClosed
	}
	for {
		if len(s.backlog) > 0 {
			ev := s.backlog[0]
			s.backlog = s.backlog[1:]
			if len(s.backlog) == 0 {
				s.backlog = nil // release the page's array
			}
			return ev, nil, nil
		}
		h.mu.Lock()
		oldest, closed := h.oldest(), h.closed
		if !s.paging || s.next >= oldest {
			s.paging = false
			ev, wait, err := s.ringLocked()
			h.mu.Unlock()
			return ev, wait, err
		}
		h.mu.Unlock()
		if closed {
			return Event{}, nil, ErrClosed
		}
		if err := s.page(ctx, oldest-1); err != nil {
			return Event{}, nil, err
		}
	}
}

// ringLocked returns the next event in scope from the ring, or the channel a
// publish closes when there is none yet.
func (s *Subscription) ringLocked() (Event, <-chan struct{}, error) {
	h := s.hub
	if s.next < h.oldest() {
		return Event{}, nil, ErrResync
	}
	for s.next <= h.lastSeq {
		ev := h.buf[h.slot(s.next)]
		s.next++
		// A stale slot is a seq nobody published under.
		if ev.Seq == s.next-1 && inScope(s.scope, ev.Path) {
			return ev, nil, nil
		}
	}
	if h.closed {
		return Event{}, nil, ErrClosed
	}
	return Event{}, h.notify, nil
}

// inScope reports whether path is under scope.
func inScope(scope, path string) bool {
	switch {
	case scope == "/":
		return true
	case strings.HasSuffix(scope, "/"):
		return strings.HasPrefix(path, scope)
	default:
		return path == scope
	}
}
