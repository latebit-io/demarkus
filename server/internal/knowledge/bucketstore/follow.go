package bucketstore

import (
	"context"
	"time"
)

// followInterval bounds how late a watcher on this replica learns of a
// peer's commit when its hint was lost: the backstop.
const followInterval = 5 * time.Second

// follower polls the bucket for what peers committed: at once on a peer's
// hint, and on a timer while a watcher waits on the hub.
type follower struct {
	kick chan struct{}
	stop context.CancelFunc
	done chan struct{}
}

func (store *Store) startFollowing(interval time.Duration) {
	ctx, stop := context.WithCancel(context.Background())
	f := &follower{kick: make(chan struct{}, 1), stop: stop, done: make(chan struct{})}
	store.follower = f
	go store.follow(ctx, f, interval)
}

func (store *Store) follow(ctx context.Context, f *follower, interval time.Duration) {
	defer close(f.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.kick:
		case <-ticker.C:
			if store.changes.Waiting() == 0 {
				continue
			}
		}
		if err := store.poll(ctx); err != nil && ctx.Err() == nil {
			store.logger.Warn("change poll failed", "error", err)
		}
	}
}

// Follow is a peer's hint that the log reached sequence (backend.Follower):
// the store polls now unless it serves it. A hint inside this store's own
// slot is a peer that lost it, so the next win hands the following one over.
func (store *Store) Follow(sequence int64) {
	if store.wrote(sequence) {
		store.waiting.Store(&peerWaiting{at: time.Now(), peerSlots: store.peerSlots.Load()})
		return
	}
	if store.servedSequence() >= sequence {
		return
	}
	f := store.follower
	if f == nil {
		return
	}
	select {
	case f.kick <- struct{}{}:
		store.logger.Info("peer hint", "sequence", sequence)
	default:
	}
}

// sequenceRange is the sequences one slot holds.
type sequenceRange struct{ first, last int64 }

// remember records a slot this store created, replacing its oldest.
func (store *Store) remember(first, last int64) {
	store.ownMu.Lock()
	defer store.ownMu.Unlock()
	store.own[store.ownNext] = sequenceRange{first: first, last: last}
	store.ownNext = (store.ownNext + 1) % ownSlots
}

// wrote reports whether sequence lies in one of this store's newest slots,
// the one being created included: a loser can hear of it first.
func (store *Store) wrote(sequence int64) bool {
	if pending := store.pending.Load(); pending != nil && pending.base.Sequence < sequence && sequence <= pending.next.Sequence {
		return true
	}
	store.ownMu.Lock()
	defer store.ownMu.Unlock()
	for _, slot := range store.own[:] {
		if slot.first > 0 && slot.first <= sequence && sequence <= slot.last {
			return true
		}
	}
	return false
}
