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

	mu      sync.Mutex
	buf     []Event
	backlog Backlog
	lastSeq uint64
	floor   uint64 // seqs at or below it cannot be resumed from: see Skip
	notify  chan struct{}
	closed  bool
}

// Backlog is a store's durable record of events, shared by every replica
// that opens the store, so this hub can lag what peers committed.
type Backlog interface {
	// Events returns every event with after < Seq <= through, in order, or
	// an error when it cannot name them all.
	Events(ctx context.Context, after, through uint64) ([]Event, error)
	// CatchUp publishes to the hub what peer replicas committed since this
	// replica last looked.
	CatchUp(ctx context.Context) error
}

// New makes a hub. An empty epoch gets a random one, which is right for a
// store whose sequence restarts with the process. ringSize <= 0 takes the
// default.
func New(epoch string, ringSize int) *Hub {
	return NewWithBacklog(epoch, ringSize, nil)
}

// NewWithBacklog is New for a store that keeps events past the ring: a
// resume the ring cannot serve, within a ring's length of the head, reads
// the rest from backlog. Nil is New.
func NewWithBacklog(epoch string, ringSize int, backlog Backlog) *Hub {
	if epoch == "" {
		epoch = NewEpoch()
	}
	if ringSize <= 0 {
		ringSize = DefaultRingSize
	}
	return &Hub{epoch: epoch, buf: make([]Event, ringSize), backlog: backlog, notify: make(chan struct{})}
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

// RingSize is how many events the hub retains for resume.
func (h *Hub) RingSize() int { return len(h.buf) }

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
	h.buf[h.slot(ev.Seq)] = ev
	h.lastSeq = ev.Seq
	h.wake()
}

func (h *Hub) wake() {
	close(h.notify)
	h.notify = make(chan struct{})
}

func (h *Hub) slot(seq uint64) uint64 { return seq % uint64(len(h.buf)) }

// oldest is the Seq of the oldest event still in the ring, or the first
// after a skip.
func (h *Hub) oldest() uint64 {
	oldest := uint64(1)
	if n := uint64(len(h.buf)); h.lastSeq > n {
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

// Subscription reads one scope's events from the hub in order: first any
// backlog loaded at Subscribe, then the ring.
type Subscription struct {
	hub     *Hub
	scope   string
	backlog []Event // unread in-scope events older than the ring, in order
	next    uint64  // Seq of the next event to read from the ring
}

// Subscribe starts a subscription over scope ("/" for everything, a prefix
// ending in "/" for a subtree, else one document) at the head, or after
// since, or ErrResync. Only a resume older than the ring does backlog I/O.
func (h *Hub) Subscribe(ctx context.Context, scope string, since protocol.Cursor) (*Subscription, error) {
	if err := h.catchUp(ctx, since); err != nil {
		return nil, err
	}
	h.mu.Lock()
	head, oldest := h.lastSeq, h.oldest()
	h.mu.Unlock()
	switch {
	case since.IsZero():
		return &Subscription{hub: h, scope: scope, next: head + 1}, nil
	case since.Epoch != h.epoch || since.Seq > head:
		return nil, ErrResync
	case since.Seq+1 >= oldest:
		// Next resyncs if the ring passes since before the first read.
		return &Subscription{hub: h, scope: scope, next: since.Seq + 1}, nil
	case h.backlog == nil || head-since.Seq > uint64(len(h.buf)):
		return nil, ErrResync
	}
	return h.resume(ctx, scope, since.Seq, oldest-1)
}

// catchUp asks the store once for peer commits when since is past this
// hub's head: a resume that moved replicas is not a gap.
func (h *Hub) catchUp(ctx context.Context, since protocol.Cursor) error {
	if h.backlog == nil || since.IsZero() || since.Epoch != h.epoch {
		return nil
	}
	h.mu.Lock()
	behind := since.Seq > h.lastSeq
	h.mu.Unlock()
	if !behind {
		return nil
	}
	if err := h.backlog.CatchUp(ctx); err != nil {
		return fmt.Errorf("%w: catch up: %w", ErrResync, err)
	}
	return nil
}

// resume loads (after, through] from the backlog outside the lock, then
// joins the ring at through+1 unless the ring moved past it meanwhile.
func (h *Hub) resume(ctx context.Context, scope string, after, through uint64) (*Subscription, error) {
	events, err := h.backlog.Events(ctx, after, through)
	if err != nil {
		return nil, fmt.Errorf("%w: backlog: %w", ErrResync, err)
	}
	want := after
	for _, ev := range events {
		want++
		if ev.Seq != want {
			return nil, fmt.Errorf("%w: backlog event has seq %d, want %d", ErrResync, ev.Seq, want)
		}
	}
	if want != through {
		return nil, fmt.Errorf("%w: backlog ended at seq %d, want %d", ErrResync, want, through)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if through+1 < h.oldest() {
		return nil, fmt.Errorf("%w: ring passed seq %d during the backlog read", ErrResync, through+1)
	}
	// A fresh slice, so an idle narrow watch holds only its own events.
	var kept []Event
	for _, ev := range events {
		if inScope(scope, ev.Path) {
			kept = append(kept, ev)
		}
	}
	return &Subscription{hub: h, scope: scope, backlog: kept, next: through + 1}, nil
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
// ErrResync once the ring has overwritten unread events, ErrClosed after
// Close, or ctx's error.
func (s *Subscription) Next(ctx context.Context) (Event, error) {
	for {
		ev, wait, err := s.pending()
		if err != nil || wait == nil {
			return ev, err
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return Event{}, ctx.Err()
		}
	}
}

// TryNext is Next without the wait: ErrIdle when nothing in scope is
// published yet.
func (s *Subscription) TryNext() (Event, error) {
	ev, wait, err := s.pending()
	if err == nil && wait != nil {
		return Event{}, ErrIdle
	}
	return ev, err
}

// ErrIdle is TryNext's answer when the subscription is at the head.
var ErrIdle = errors.New("changefeed: no event pending")

// pending returns the next event in scope, or the channel a publish closes
// when there is none yet.
func (s *Subscription) pending() (Event, <-chan struct{}, error) {
	if len(s.backlog) > 0 {
		ev := s.backlog[0]
		s.backlog = s.backlog[1:]
		if len(s.backlog) == 0 {
			s.backlog = nil // release the loaded array
		}
		return ev, nil, nil
	}
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
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
