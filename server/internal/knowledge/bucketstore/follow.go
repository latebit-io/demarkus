package bucketstore

import (
	"context"
	"time"
)

// followInterval bounds how late a watcher on this replica learns of a
// peer's commit when its hint was lost: the backstop.
const followInterval = 5 * time.Second

// follower polls the bucket for what peers committed: at once on a peer's
// hint, and on a timer while the hub has subscribers.
type follower struct {
	kick chan struct{}
	stop context.CancelFunc
	done chan struct{}
}

func (store *Store) startFollowing(interval time.Duration) {
	if interval <= 0 {
		interval = followInterval
	}
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
			if store.changes.Subscribers() == 0 {
				continue
			}
		}
		if err := store.poll(ctx); err != nil && ctx.Err() == nil {
			store.logger.Warn("change poll failed", "error", err)
		}
	}
}

// Follow is a peer replica's hint that it committed through sequence
// (backend.Follower): the store polls now unless it already serves it.
// Without WATCH every read validates the head, so a hint adds nothing.
func (store *Store) Follow(sequence int64) {
	f := store.follower
	if f == nil || store.servedSequence() >= sequence {
		return
	}
	select {
	case f.kick <- struct{}{}:
		store.logger.Info("peer hint", "sequence", sequence)
	default:
	}
}

// Close stops following peers and ends every watch on the hub; reads and
// writes still work. It is idempotent.
func (store *Store) Close() error {
	if f := store.follower; f != nil {
		f.stop()
		<-f.done
	}
	if store.changes != nil {
		store.changes.Close()
	}
	return nil
}
