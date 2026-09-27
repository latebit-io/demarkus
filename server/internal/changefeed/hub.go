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
	"sort"
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

// Event is one committed change. Seq is assigned by the hub on Publish.
type Event struct {
	Seq     uint64
	Path    string
	Version int
	Hash    string
	Op      string
	Agent   string
}

// Hub holds a world's recent events and wakes subscribers on each publish.
type Hub struct {
	epoch string

	mu      sync.Mutex
	buf     []Event
	written int // events appended since the hub was made
	lastSeq uint64
	// lastDropped is the Seq of the newest event the ring overwrote; a cursor
	// below it may be missing events and gets resync.
	lastDropped uint64
	notify      chan struct{}
	closed      bool
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

// PublishAt appends ev under its own Seq, which must exceed the last one; a
// replica feeding events from a shared sequence uses it.
func (h *Hub) PublishAt(ev Event) (protocol.Cursor, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ev.Seq <= h.lastSeq {
		return protocol.Cursor{}, errors.New("changefeed: sequence must increase")
	}
	h.append(ev)
	return h.cursor(ev.Seq), nil
}

func (h *Hub) append(ev Event) {
	n := len(h.buf)
	if h.written >= n {
		h.lastDropped = h.buf[h.written%n].Seq
	}
	h.buf[h.written%n] = ev
	h.written++
	h.lastSeq = ev.Seq
	close(h.notify)
	h.notify = make(chan struct{})
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
	hub    *Hub
	scope  string
	pos    int // ring position of the next event to read
	cursor protocol.Cursor
}

// Subscribe starts a subscription over scope ("/" for everything, a prefix
// ending in "/" for a subtree, else one document). A zero cursor starts at
// the head; otherwise delivery resumes after since, or ErrResync.
func (h *Hub) Subscribe(scope string, since protocol.Cursor) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &Subscription{hub: h, scope: scope}
	if since.IsZero() {
		s.pos = h.written
		s.cursor = h.cursor(h.lastSeq)
		return s, nil
	}
	if since.Epoch != h.epoch || since.Seq > h.lastSeq || since.Seq < h.lastDropped {
		return nil, ErrResync
	}
	oldest := h.oldestPos()
	// Retained events are sorted by Seq; find the first one after since.
	i := sort.Search(h.written-oldest, func(i int) bool {
		return h.buf[(oldest+i)%len(h.buf)].Seq > since.Seq
	})
	s.pos = oldest + i
	s.cursor = since
	return s, nil
}

// oldestPos is the ring position of the oldest retained event.
func (h *Hub) oldestPos() int {
	if n := len(h.buf); h.written > n {
		return h.written - n
	}
	return 0
}

// Cursor is the position to resume from: the last event consumed, matching
// or not, so a resume replays as little as possible.
func (s *Subscription) Cursor() protocol.Cursor { return s.cursor }

// Next returns the next event in scope, waiting for one. It returns
// ErrResync once the ring has overwritten unread events, ErrClosed after
// Close, or ctx's error.
func (s *Subscription) Next(ctx context.Context) (Event, error) {
	h := s.hub
	for {
		h.mu.Lock()
		if s.pos < h.oldestPos() {
			h.mu.Unlock()
			return Event{}, ErrResync
		}
		for s.pos < h.written {
			ev := h.buf[s.pos%len(h.buf)]
			s.pos++
			s.cursor = h.cursor(ev.Seq)
			if inScope(s.scope, ev.Path) {
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
