package bucketstore

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

const (
	// backlogReach is how many slots behind the head a resume may start, so
	// one resume costs the bucket at most that many reads (ADR 0036).
	backlogReach = 1 << 14
	// slotCacheIdle is how long the slot cache outlives its last read.
	slotCacheIdle = 30 * time.Minute
)

// errBeyondReach refuses a resume further behind than the backlog serves.
var errBeyondReach = errors.New("past the change backlog's reach")

// changeLog is the log's slots as the hub's backlog. Slots are immutable, so
// recently read ones stay cached (LRU, at most a ring of events, emptied when
// idle): a burst of resumes, honest or not, costs one read per slot.
type changeLog struct {
	store *Store
	reach uint64 // slots
	idle  time.Duration

	mu      sync.Mutex
	limit   int // cached events
	held    int
	recent  *list.List // front is the most recently used *cachedSlot
	slots   map[int64]*list.Element
	used    time.Time   // the last read
	evict   *time.Timer // armed while the cache holds anything
	closed  bool
	reading map[int64]*slotFlight // reads in flight, which callers share
}

// slotFlight is one slot read that concurrent callers wait on.
type slotFlight struct {
	done   chan struct{}
	events []changefeed.Event
	err    error
}

type cachedSlot struct {
	first  int64
	events []changefeed.Event
}

var _ changefeed.Backlog = (*changeLog)(nil)

func newChangeLog(store *Store, ring int, reach uint64, idle time.Duration) *changeLog {
	return &changeLog{
		store:   store,
		reach:   reach,
		idle:    idle,
		limit:   max(ring, maxSlotEntries),
		recent:  list.New(),
		slots:   make(map[int64]*list.Element),
		reading: make(map[int64]*slotFlight),
	}
}

// CatchUp reads the slots peers committed since this replica last looked.
func (backlog *changeLog) CatchUp(ctx context.Context) error { return backlog.store.poll(ctx) }

// slot returns the events of the slot named first: cached, from a read in
// flight, or read now. A waiter whose flight failed reads again itself, so
// one caller's cancellation never fails another.
func (backlog *changeLog) slot(ctx context.Context, first int64) ([]changefeed.Event, error) {
	for {
		backlog.mu.Lock()
		backlog.used = backlog.store.now()
		if element, ok := backlog.slots[first]; ok {
			backlog.recent.MoveToFront(element)
			backlog.mu.Unlock()
			return element.Value.(*cachedSlot).events, nil
		}
		flight, waiting := backlog.reading[first]
		if !waiting {
			flight = &slotFlight{done: make(chan struct{})}
			backlog.reading[first] = flight
		}
		backlog.mu.Unlock()
		if !waiting {
			return backlog.fly(ctx, first, flight)
		}
		select {
		case <-flight.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if flight.err == nil {
			return flight.events, nil
		}
	}
}

// fly reads the slot named first for flight and caches it.
func (backlog *changeLog) fly(ctx context.Context, first int64, flight *slotFlight) ([]changefeed.Event, error) {
	read, err := readSlot(ctx, backlog.store.objects, backlog.store.worldID, first)
	if err == nil {
		flight.events = slotEvents(read.slot)
	}
	flight.err = err
	backlog.mu.Lock()
	delete(backlog.reading, first)
	if err == nil {
		backlog.rememberLocked(first, flight.events)
	}
	backlog.mu.Unlock()
	close(flight.done)
	return flight.events, err
}

func (backlog *changeLog) rememberLocked(first int64, events []changefeed.Event) {
	if backlog.closed {
		return
	}
	backlog.slots[first] = backlog.recent.PushFront(&cachedSlot{first: first, events: events})
	backlog.held += len(events)
	if backlog.evict == nil {
		backlog.evict = time.AfterFunc(backlog.idle, backlog.evictIfIdle)
	}
	for backlog.held > backlog.limit {
		oldest := backlog.recent.Back()
		evicted := oldest.Value.(*cachedSlot)
		delete(backlog.slots, evicted.first)
		backlog.held -= len(evicted.events)
		backlog.recent.Remove(oldest)
	}
}

// evictIfIdle empties the cache once it has gone unread for idle, so a
// world nobody resumes on holds no events.
func (backlog *changeLog) evictIfIdle() {
	backlog.mu.Lock()
	defer backlog.mu.Unlock()
	if backlog.evict == nil {
		return // emptied since the timer fired
	}
	if wait := backlog.idle - backlog.store.now().Sub(backlog.used); wait > 0 {
		backlog.evict.Reset(wait)
		return
	}
	backlog.dropLocked()
}

// dropLocked empties the cache and disarms its timer.
func (backlog *changeLog) dropLocked() {
	if backlog.evict != nil {
		backlog.evict.Stop()
		backlog.evict = nil
	}
	backlog.recent, backlog.slots, backlog.held = list.New(), make(map[int64]*list.Element), 0
}

// close empties the cache for good.
func (backlog *changeLog) close() {
	backlog.mu.Lock()
	defer backlog.mu.Unlock()
	backlog.closed = true
	backlog.dropLocked()
}

// Reaches refuses a resume more than reach slots behind head. A slot holds 1
// to maxSlotEntries changes, so only a lag between those bounds is listed.
func (backlog *changeLog) Reaches(ctx context.Context, after, head uint64) error {
	reach := backlog.reach
	switch lag := head - after; {
	case lag <= reach:
		return nil
	case lag > reach*maxSlotEntries:
		return fmt.Errorf("%w: %d changes behind, more than %d slots", errBeyondReach, lag, reach)
	}
	first, last, err := logRange(after, head)
	if err != nil {
		return err
	}
	// One slot holds first; each name after it is one more. A missing slot
	// is Events' to find.
	slots := uint64(1)
	for _, err := range slotNames(ctx, backlog.store.objects, first, last) {
		if err != nil {
			return err
		}
		slots++
		if slots > reach {
			return fmt.Errorf("%w: more than %d slots behind", errBeyondReach, reach)
		}
	}
	return nil
}

// Events reads the slots holding (after, through]. A missing slot, or one
// that fails verification, fails the read: the watcher resyncs.
func (backlog *changeLog) Events(ctx context.Context, after, through uint64) ([]changefeed.Event, error) {
	if through <= after {
		return nil, nil
	}
	first, last, err := logRange(after, through)
	if err != nil {
		return nil, err
	}
	events, err := backlog.read(ctx, first, last)
	if err != nil {
		level := slog.LevelWarn
		switch {
		case errors.Is(err, blob.ErrIntegrity):
			level = slog.LevelError
		case errors.Is(err, blob.ErrNotFound):
			level = slog.LevelInfo
		}
		backlog.store.logger.Log(ctx, level, "change backlog unavailable; watcher resyncs", "world", backlog.store.worldID, "after", after, "through", through, "error", err)
		return nil, err
	}
	return events, nil
}

// read returns the events [first, last] from the slots holding them: the
// slot holding first starts within maxSlotEntries names at or below it.
func (backlog *changeLog) read(ctx context.Context, first, last int64) ([]changefeed.Event, error) {
	var names []int64
	for name, err := range slotNames(ctx, backlog.store.objects, max(first-maxSlotEntries, 1), last) {
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	// Names are first sequences, so the slot holding first is the last name
	// at or below it; earlier names hold nothing in range.
	for len(names) > 1 && names[1] <= first {
		names = names[1:]
	}
	slots := make([][]changefeed.Event, len(names))
	err := runParallel(ctx, backlog.store.shardWorkers, indexes(len(names)), func(ctx context.Context, index int) error {
		events, err := backlog.slot(ctx, names[index])
		slots[index] = events
		return err
	})
	if err != nil {
		return nil, err
	}
	events := make([]changefeed.Event, 0, last-first+1)
	next := hubSeq(first)
	for _, slot := range slots {
		if slot[0].Seq > next {
			return nil, fmt.Errorf("%w: no slot holds sequence %d", blob.ErrNotFound, next)
		}
		for _, event := range slot {
			if event.Seq >= hubSeq(first) && event.Seq <= hubSeq(last) {
				events = append(events, event)
			}
		}
		next = max(next, slot[len(slot)-1].Seq+1)
	}
	if next <= hubSeq(last) {
		return nil, fmt.Errorf("%w: no slot holds sequence %d", blob.ErrNotFound, next)
	}
	return events, nil
}

// slotNames yields the names of the slots after `after` up to last, in order.
func slotNames(ctx context.Context, objects blob.Store, after, last int64) iter.Seq2[int64, error] {
	return func(yield func(int64, error) bool) {
		for firsts, err := range sequencePages(ctx, objects, logPrefix, after) {
			if err != nil {
				yield(0, err)
				return
			}
			for _, name := range firsts {
				if name > last || !yield(name, nil) {
					return
				}
			}
		}
	}
}

// logRange is the hub range (after, through] as log sequences [first, last].
func logRange(after, through uint64) (first, last int64, err error) {
	if first, err = logSequence(after + 1); err == nil {
		last, err = logSequence(through)
	}
	return first, last, err
}

// logSequence is a hub sequence as a log sequence, the inverse of hubSeq.
func logSequence(seq uint64) (int64, error) {
	if seq > math.MaxInt64 {
		return 0, fmt.Errorf("sequence %d is past any log", seq)
	}
	return int64(seq), nil
}
