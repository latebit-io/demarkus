// Package fanout serves every WATCH of a world from one hub subscription.
// An event is matched, authorized and encoded once per group of watchers
// that share a scope and a token; each watcher then copies the shared bytes
// from a ring at its own pace, so a slow stream never holds the reader.
package fanout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// DefaultMaxWatches caps a world's open watches when the config leaves it zero.
const DefaultMaxWatches = 1024

// Config builds a Fanout. Hub and Logger are required; a nil Tokens means
// every path is public.
type Config struct {
	Hub    *changefeed.Hub
	Tokens func() *auth.TokenStore
	Logger *slog.Logger
	// Heartbeat paces idle streams; zero is the protocol's interval.
	Heartbeat time.Duration
	// MaxWatches caps open watches per world; zero takes the default.
	// MaxWatchesPerConn caps them per connection; zero leaves no cap.
	MaxWatches        int
	MaxWatchesPerConn int
}

// Fanout is one world's watch multiplexer. It holds nothing while no watch
// is open: the reader, the sweep and the ring exist only between the first
// attach and the last detach.
type Fanout struct {
	hub        *changefeed.Hub
	tokens     func() *auth.TokenStore
	logger     *slog.Logger
	heartbeat  time.Duration
	maxWatches int
	maxPerConn int

	mu       sync.Mutex
	watchers int
	groups   map[groupKey]*group
	byScope  map[string][]*group
	lastID   uint32
	run      *run
}

// New validates config; the fanout is idle until the first Serve.
func New(config Config) (*Fanout, error) {
	if config.Hub == nil {
		return nil, errors.New("fanout: hub is nil")
	}
	if config.Logger == nil {
		return nil, errors.New("fanout: logger is nil")
	}
	if config.MaxWatches < 0 || config.MaxWatchesPerConn < 0 {
		return nil, fmt.Errorf("fanout: limits must not be negative: %d, %d", config.MaxWatches, config.MaxWatchesPerConn)
	}
	f := &Fanout{
		hub:        config.Hub,
		tokens:     config.Tokens,
		logger:     config.Logger,
		heartbeat:  config.Heartbeat,
		maxWatches: config.MaxWatches,
		maxPerConn: config.MaxWatchesPerConn,
		groups:     map[groupKey]*group{},
		byScope:    map[string][]*group{},
	}
	if f.heartbeat <= 0 {
		f.heartbeat = protocol.WatchHeartbeatInterval
	}
	if f.maxWatches == 0 {
		f.maxWatches = DefaultMaxWatches
	}
	return f, nil
}

// Watches reports the open watches.
func (f *Fanout) Watches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watchers
}

func (f *Fanout) tokenStore() *auth.TokenStore {
	if f.tokens == nil {
		return nil
	}
	return f.tokens()
}

func (f *Fanout) cursor(seq uint64) protocol.Cursor {
	return protocol.Cursor{Epoch: f.hub.Epoch(), Seq: seq}
}

// groupKey names what a permission check depends on: the scope watched and
// the token (hashed, or empty) it is watched with.
type groupKey struct{ scope, token string }

// group is every watcher that shares a key: one id per event marks the
// entries they may read, one channel wakes only them.
type group struct {
	id       uint32
	key      groupKey
	watchers int
	notify   chan struct{}
	// from is the first seq matched against this group; entries before it
	// never name it, so a resume from earlier goes through the hub.
	from uint64
	// denied is set by the sweep when the token no longer covers the
	// scope; its watchers end with that verdict.
	denied error
}

func (g *group) wake() {
	close(g.notify)
	g.notify = make(chan struct{})
}

// entry is one encoded event in the ring. block is nil when no group had
// the event, so unwatched paths cost only the slot.
type entry struct {
	seq    uint64
	path   string
	block  []byte
	groups []uint32
}

// run is the live state while watchers exist. Indexes are sequence
// numbers; slot is seq modulo the ring, as in the hub.
type run struct {
	cancel  context.CancelFunc
	done    sync.WaitGroup
	buf     []entry
	next    uint64 // seq the reader appends next
	floor   uint64 // an index at or below it cannot continue: see resync
	tick    uint64 // sweeps so far; a watcher idle across one heartbeats
	closed  bool   // the hub closed; watchers end with closing
	matched []*group
}

// oldest is the lowest index a watcher may still read from.
func (r *run) oldest() uint64 {
	oldest := r.floor + 1
	if n := uint64(len(r.buf)); r.next > n {
		oldest = max(oldest, r.next-n)
	}
	return oldest
}

func (r *run) slot(seq uint64) uint64 { return seq % uint64(len(r.buf)) }

// startLocked subscribes at the hub's head and starts the reader and the
// sweep; the ring is sized like the hub's so both lap a watcher together.
func (f *Fanout) startLocked() (*run, error) {
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := f.hub.Subscribe(ctx, "/", protocol.Cursor{})
	if err != nil {
		cancel()
		return nil, err
	}
	// The subscription's own position, not a second Head read: a write in
	// between would put the run ahead of the events the reader delivers.
	head := sub.Cursor().Seq
	r := &run{cancel: cancel, buf: make([]entry, f.hub.RingSize()), next: head + 1, floor: head}
	f.run = r
	r.done.Add(2)
	go f.read(ctx, r, sub)
	go f.sweep(ctx, r)
	return r, nil
}

// stopLocked drops the run; the goroutines see f.run change and exit.
func (f *Fanout) stopLocked() *run {
	r := f.run
	f.run = nil
	r.cancel()
	return r
}

func (f *Fanout) wakeAllLocked() {
	for _, g := range f.groups {
		g.wake()
	}
}

// read is the one hub subscriber: it appends every event to the ring. A
// hub resync moves the floor over everything read so far and rejoins at
// the head; a hub close ends every watcher with closing.
func (f *Fanout) read(ctx context.Context, r *run, sub *changefeed.Subscription) {
	defer r.done.Done()
	for {
		ev, err := sub.Next(ctx)
		switch {
		case err == nil:
			f.appendEvent(r, ev)
			continue
		case ctx.Err() != nil:
			return
		case errors.Is(err, changefeed.ErrResync):
			if sub, err = f.hub.Subscribe(ctx, "/", protocol.Cursor{}); err == nil {
				f.rejoin(r, sub.Cursor().Seq)
				continue
			}
			f.logger.Error("watch fan-out lost the hub", "error", err)
		case !errors.Is(err, changefeed.ErrClosed):
			f.logger.Error("watch fan-out failed", "error", err)
		}
		f.end(r)
		return
	}
}

func (f *Fanout) rejoin(r *run, head uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.run == r {
		r.floor, r.next = head, head+1
		f.wakeAllLocked()
	}
	f.logger.Info("watch fan-out resync", "head", head)
}

func (f *Fanout) end(r *run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.run == r {
		r.closed = true
		f.wakeAllLocked()
	}
}

// appendEvent matches the event against the groups whose scope covers its
// path, checks each group's token once, encodes the block once when any
// may read it, and wakes those groups alone.
func (f *Fanout) appendEvent(r *run, ev changefeed.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.run != r {
		return
	}
	e := entry{seq: ev.Seq, path: ev.Path}
	if matched := f.matchLocked(r, ev.Path); len(matched) > 0 {
		e.groups = make([]uint32, len(matched))
		for i, g := range matched {
			e.groups[i] = g.id
			g.wake()
		}
		e.block = f.encode(ev)
	}
	r.buf[r.slot(ev.Seq)] = e
	r.next = ev.Seq + 1
}

// matchLocked walks the path's prefixes ("/", each directory, the path
// itself) and returns the groups there whose token reads it, in r's
// scratch; a public path skips the per-group check.
func (f *Fanout) matchLocked(r *run, path string) []*group {
	r.matched = r.matched[:0]
	store := f.tokenStore()
	protected := store.RequiresReadAuth(path)
	collect := func(scope string) {
		for _, g := range f.byScope[scope] {
			if g.denied == nil && (!protected || store.AuthorizeReadHashed(g.key.token, path) == nil) {
				r.matched = append(r.matched, g)
			}
		}
	}
	collect("/")
	for i := 1; i < len(path); i++ {
		if path[i] == '/' {
			collect(path[:i+1])
		}
	}
	if !strings.HasSuffix(path, "/") {
		collect(path)
	}
	return r.matched
}

// encode renders the event once; a block the codec refuses is skipped with
// a warning, as nothing could carry it.
func (f *Fanout) encode(ev changefeed.Event) []byte {
	var buf bytes.Buffer
	if _, err := ev.Block(f.hub.Epoch()).WriteTo(&buf); err != nil {
		f.logger.Warn("watch block skipped", "path", ev.Path, "error", err)
		return nil
	}
	return buf.Bytes()
}

// sweep ticks once per heartbeat interval: it rechecks each group's token
// against the current store and wakes every watcher, which heartbeats when
// it wrote nothing since the last tick.
func (f *Fanout) sweep(ctx context.Context, r *run) {
	defer r.done.Done()
	ticker := time.NewTicker(f.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		f.mu.Lock()
		if f.run != r {
			f.mu.Unlock()
			return
		}
		store := f.tokenStore()
		for _, g := range f.groups {
			if g.denied == nil {
				g.denied = store.AuthorizeReadHashed(g.key.token, g.key.scope)
			}
		}
		r.tick++
		f.wakeAllLocked()
		f.mu.Unlock()
	}
}

// joinLocked adds a watcher to its group, creating it on first use at the
// ring's next seq.
func (f *Fanout) joinLocked(key groupKey, next uint64) *group {
	g := f.groups[key]
	if g == nil {
		f.lastID++
		g = &group{id: f.lastID, key: key, notify: make(chan struct{}), from: next}
		f.groups[key] = g
		f.byScope[key.scope] = append(f.byScope[key.scope], g)
	}
	g.watchers++
	return g
}

// leaveLocked removes a watcher; an empty group is dropped, so idle scopes
// and tokens hold nothing.
func (f *Fanout) leaveLocked(g *group) {
	g.watchers--
	if g.watchers > 0 {
		return
	}
	delete(f.groups, g.key)
	peers := slices.DeleteFunc(f.byScope[g.key.scope], func(other *group) bool { return other == g })
	if len(peers) == 0 {
		delete(f.byScope, g.key.scope)
	} else {
		f.byScope[g.key.scope] = peers
	}
}
