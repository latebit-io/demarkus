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

// Event is one committed change. Seq is assigned by the hub on Publish, or
// by the caller on PublishAt.
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
	lastSeq uint64
	floor   uint64 // seqs at or below it cannot be resumed from: see Skip
	notify  chan struct{}
	closed  bool
}

// New makes a hub. An empty epoch gets a random one, which is right for a
// store whose sequence restarts with the process. ringSize <= 0 takes the
// default.
func New(epoch string, ringSize int) *Hub {
	if epoch == "" {
		epoch = NewEpoch()
	}
	if ringSize <= 0 {
		ringSize = DefaultRingSize
	}
	return &Hub{epoch: epoch, buf: make([]Event, ringSize), notify: make(chan struct{})}
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

// Head is the cursor of the newest event, or seq 0 before any.
func (h *Hub) Head() protocol.Cursor {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cursor(h.lastSeq)
}

func (h *Hub) cursor(seq uint64) protocol.Cursor {
	return protocol.Cursor{Epoch: h.epoch, Seq: seq}
}

// Publish appends ev with the next sequence number and wakes subscribers.
// It never blocks on a reader. Returns the event's cursor.
func (h *Hub) Publish(ev Event) protocol.Cursor {
	h.mu.Lock()
	defer h.mu.Unlock()
	ev.Seq = h.lastSeq + 1
	h.append(ev)
	return h.cursor(ev.Seq)
}

// PublishAt appends ev under its own Seq, for a store whose commits carry a
// shared sequence. A Seq the hub already passed is dropped, reported false:
// the same commit reached the hub twice.
func (h *Hub) PublishAt(ev Event) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ev.Seq <= h.lastSeq {
		return false
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

// Subscription reads one scope's events from the hub in order.
type Subscription struct {
	hub   *Hub
	scope string
	next  uint64 // Seq of the next event to read
}

// Subscribe starts a subscription over scope ("/" for everything, a prefix
// ending in "/" for a subtree, else one document). A zero cursor starts at
// the head; otherwise delivery resumes after since, or ErrResync.
func (h *Hub) Subscribe(scope string, since protocol.Cursor) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if since.IsZero() {
		return &Subscription{hub: h, scope: scope, next: h.lastSeq + 1}, nil
	}
	if since.Epoch != h.epoch || since.Seq > h.lastSeq || since.Seq+1 < h.oldest() {
		return nil, ErrResync
	}
	return &Subscription{hub: h, scope: scope, next: since.Seq + 1}, nil
}

// Scope is what the subscription was opened over.
func (s *Subscription) Scope() string { return s.scope }

// Cursor is the position to resume from: the last event consumed, matching
// or not, so a resume replays as little as possible.
func (s *Subscription) Cursor() protocol.Cursor { return s.hub.cursor(s.next - 1) }

// Next returns the next event in scope, waiting for one. It returns
// ErrResync once the ring has overwritten unread events, ErrClosed after
// Close, or ctx's error.
func (s *Subscription) Next(ctx context.Context) (Event, error) {
	h := s.hub
	for {
		h.mu.Lock()
		if s.next < h.oldest() {
			h.mu.Unlock()
			return Event{}, ErrResync
		}
		for s.next <= h.lastSeq {
			ev := h.buf[h.slot(s.next)]
			s.next++
			// A stale slot is a seq nobody published under.
			if ev.Seq == s.next-1 && inScope(s.scope, ev.Path) {
				h.mu.Unlock()
				return ev, nil
			}
		}
		if h.closed {
			h.mu.Unlock()
			return Event{}, ErrClosed
		}
		wait := h.notify
		h.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return Event{}, ctx.Err()
		}
	}
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
