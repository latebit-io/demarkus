package bucketstore

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// changeLog is the log's slots as the hub's backlog. Slots are immutable, so
// the events of recently read ones stay cached (LRU, at most a ring of
// events): a burst of resumes, honest or not, costs one read per slot.
type changeLog struct {
	store *Store

	mu     sync.Mutex
	limit  int // cached events
	held   int
	recent *list.List // front is the most recently used *cachedSlot
	slots  map[int64]*list.Element
}

type cachedSlot struct {
	first  int64
	events []changefeed.Event
}

var _ changefeed.Backlog = (*changeLog)(nil)

func newChangeLog(store *Store, ring int) *changeLog {
	return &changeLog{
		store:  store,
		limit:  max(ring, maxSlotEntries),
		recent: list.New(),
		slots:  make(map[int64]*list.Element),
	}
}

// CatchUp reads the slots peers committed since this replica last looked.
func (backlog *changeLog) CatchUp(ctx context.Context) error { return backlog.store.poll(ctx) }

func (backlog *changeLog) cached(first int64) ([]changefeed.Event, bool) {
	backlog.mu.Lock()
	defer backlog.mu.Unlock()
	element, ok := backlog.slots[first]
	if !ok {
		return nil, false
	}
	backlog.recent.MoveToFront(element)
	return element.Value.(*cachedSlot).events, true
}

func (backlog *changeLog) remember(first int64, events []changefeed.Event) {
	backlog.mu.Lock()
	defer backlog.mu.Unlock()
	if element, ok := backlog.slots[first]; ok {
		backlog.recent.MoveToFront(element)
		return
	}
	backlog.slots[first] = backlog.recent.PushFront(&cachedSlot{first: first, events: events})
	backlog.held += len(events)
	for backlog.held > backlog.limit {
		oldest := backlog.recent.Back()
		evicted := oldest.Value.(*cachedSlot)
		delete(backlog.slots, evicted.first)
		backlog.held -= len(evicted.events)
		backlog.recent.Remove(oldest)
	}
}

// Events reads the slots holding (after, through]. A missing slot, or one
// that fails verification, fails the read: the watcher resyncs.
func (backlog *changeLog) Events(ctx context.Context, after, through uint64) ([]changefeed.Event, error) {
	if through <= after {
		return nil, nil
	}
	first, err := headSequence(after + 1)
	if err != nil {
		return nil, err
	}
	last, err := headSequence(through)
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
	kept := make([]changefeed.Event, 0, through-after)
	for _, event := range events {
		if event.Seq > after && event.Seq <= through {
			kept = append(kept, event)
		}
	}
	return kept, nil
}

// read returns the events of every slot that may hold [first, last]: the slot
// holding first starts within maxSlotEntries names at or below it.
func (backlog *changeLog) read(ctx context.Context, first, last int64) ([]changefeed.Event, error) {
	store := backlog.store
	var names []int64
listing:
	for firsts, err := range sequencePages(ctx, store.objects, logPrefix, max(first-maxSlotEntries, 1)) {
		if err != nil {
			return nil, err
		}
		for _, name := range firsts {
			if name > last {
				break listing
			}
			names = append(names, name)
		}
	}
	// Names are first sequences, so the slot holding first is the last name
	// at or below it; earlier names hold nothing in range.
	for len(names) > 1 && names[1] <= first {
		names = names[1:]
	}
	slots := make([][]changefeed.Event, len(names))
	var missing []int
	for index, name := range names {
		if events, ok := backlog.cached(name); ok {
			slots[index] = events
		} else {
			missing = append(missing, index)
		}
	}
	err := runParallel(ctx, store.shardWorkers, missing, func(ctx context.Context, index int) error {
		slot, _, err := readSlot(ctx, store.objects, store.worldID, names[index])
		if err != nil {
			return err
		}
		events := slotEvents(slot)
		backlog.remember(names[index], events)
		slots[index] = events
		return nil
	})
	if err != nil {
		return nil, err
	}
	var events []changefeed.Event
	next := hubSeq(first)
	for _, slot := range slots {
		if slot[0].Seq > next {
			return nil, fmt.Errorf("%w: no slot holds sequence %d", blob.ErrNotFound, next)
		}
		events = append(events, slot...)
		next = max(next, slot[len(slot)-1].Seq+1)
	}
	if next <= hubSeq(last) {
		return nil, fmt.Errorf("%w: no slot holds sequence %d", blob.ErrNotFound, next)
	}
	return events, nil
}

// headSequence is a hub sequence as a log sequence, the inverse of hubSeq.
func headSequence(seq uint64) (int64, error) {
	if seq > math.MaxInt64 {
		return 0, fmt.Errorf("sequence %d is past any log", seq)
	}
	return int64(seq), nil
}
