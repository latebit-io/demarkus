package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/latebit-io/demarkus/server/internal/backend"
)

// served is the snapshot this replica serves and when it was last confirmed
// as the log's tip: a probe that started then found no later slot.
type served struct {
	snap      *snapshot
	confirmed time.Time
}

// pendingSlot is a slot this store is creating: a refresh that reads it before
// the create returns installs next rather than applying and indexing it again.
type pendingSlot struct {
	base, next *snapshot
	hash       string
}

// readClass is how fresh a view's first read needs its snapshot.
type readClass int

const (
	// exactRead sees every slot created before the read (reads by path).
	exactRead readClass = iota
	// catalogRead may use a snapshot confirmed within SearchFreshness.
	catalogRead
)

// current returns the snapshot a view's first read pins: for a catalog read
// one confirmed within SearchFreshness, otherwise the log's tip.
func (store *Store) current(ctx context.Context, class readClass) (*snapshot, error) {
	if served := store.served.Load(); class == catalogRead && store.now().Sub(served.confirmed) < backend.SearchFreshness {
		return served.snap, nil
	}
	return store.refresh(ctx)
}

// refresh catches up to the log's tip. Probes are a shared flight: a caller
// takes a tip confirmed after it asked, waits on the probe in flight no
// longer than its own context, and otherwise starts the next one.
func (store *Store) refresh(ctx context.Context) (*snapshot, error) {
	asked := store.now()
	if asked.Sub(store.served.Load().confirmed) > staleAfter || store.diverged.Load() {
		if err := store.reload(ctx); err != nil {
			return nil, fmt.Errorf("refresh: %w", err)
		}
	}
	for {
		if served := store.served.Load(); served.confirmed.After(asked) {
			return served.snap, nil
		}
		flight, started := store.joinProbe()
		if started {
			return store.probe(ctx, flight, asked)
		}
		select {
		case <-flight.done:
			// A bucket failure answers the burst once; only a probe that ended
			// with its starter's context leaves the waiters to probe again.
			if flight.err != nil && !flight.canceled {
				return nil, flight.err
			}
		case <-ctx.Done():
			return nil, fmt.Errorf("refresh: %w", ctx.Err())
		}
	}
}

// probeFlight is one probe of the log's tip and how it ended.
type probeFlight struct {
	done     chan struct{}
	err      error
	canceled bool // the starter's context ended
}

// joinProbe returns the probe in flight, or a new one this caller runs.
func (store *Store) joinProbe() (flight *probeFlight, started bool) {
	store.probeMu.Lock()
	defer store.probeMu.Unlock()
	if store.probing == nil {
		store.probing = &probeFlight{done: make(chan struct{})}
		return store.probing, true
	}
	return store.probing, false
}

// probe runs the flight's catch-up and releases it on every exit, so a panic
// recovered above the store cannot leave later refreshes waiting on it.
func (store *Store) probe(ctx context.Context, flight *probeFlight, asked time.Time) (tip *snapshot, err error) {
	defer func() {
		flight.err, flight.canceled = err, ctx.Err() != nil
		store.probeMu.Lock()
		store.probing = nil
		store.probeMu.Unlock()
		close(flight.done)
	}()
	return store.catchUp(ctx, nil, asked)
}

// catchUpFrom catches up through a slot this store lost the race for, outside
// the probe flight: it holds the winner already and reads only the listing
// after it, so a loser's recovery never waits behind a reader's probe.
func (store *Store) catchUpFrom(ctx context.Context, winner slotRead) (*snapshot, error) {
	return store.catchUp(ctx, &winner, time.Time{})
}

// catchUp brings the served snapshot to the log's tip, from its next slot or
// from known, one this store lost the race for. A commit installing meanwhile
// restarts it, unless that one was confirmed after asked.
func (store *Store) catchUp(ctx context.Context, known *slotRead, asked time.Time) (*snapshot, error) {
	for {
		current := store.served.Load()
		if !asked.IsZero() && current.confirmed.After(asked) {
			return current.snap, nil
		}
		base := current.snap
		if known != nil && base.Sequence >= known.slot.last() {
			// A probe installed the winner's slot meanwhile and listed past it;
			// that stands as the tip, so a loser never waits on a probe.
			return base, nil
		}
		ahead, err := store.readAhead(ctx, base, known)
		if ahead.next == nil && err != nil {
			return nil, fmt.Errorf("refresh: %w", err)
		}
		store.installMu.Lock()
		if store.served.Load().snap != base {
			store.installMu.Unlock()
			continue
		}
		switch {
		case ahead.next == nil:
			store.served.Store(&served{snap: base, confirmed: ahead.confirmed})
		case err != nil:
			// Not the tip, but what applied is kept, so a replica far behind
			// gains ground on every attempt.
			store.install(ahead.next, ahead.applied, current.confirmed)
		default:
			store.install(ahead.next, ahead.applied, ahead.confirmed)
		}
		store.installMu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("refresh: %w", err)
		}
		return store.served.Load().snap, nil
	}
}

// ahead is what a catch-up read past base: next is nil when base is the tip,
// and confirmed is when the tip was observed.
type ahead struct {
	next      *snapshot
	applied   []appliedSlot
	confirmed time.Time
}

// readAhead reads the slots after base, from the bucket alone. A read that
// fails after some slots applied returns them with its error.
func (store *Store) readAhead(ctx context.Context, base *snapshot, known *slotRead) (ahead, error) {
	probed := store.now()
	var first slotRead
	if known != nil {
		first = *known
	} else {
		read, err := readSlot(ctx, store.objects, store.worldID, base.Sequence+1)
		if missing(err) {
			return ahead{confirmed: probed}, nil
		}
		if err != nil {
			return ahead{}, err
		}
		first = read
	}
	var next *snapshot
	if pending := store.pending.Load(); pending != nil && pending.hash == first.hash && pending.base.Sequence == base.Sequence {
		next = store.derive(pending.next)
	} else {
		next = store.derive(base)
		if err := next.applySlot(first); err != nil {
			return ahead{}, err
		}
	}
	applied := []appliedSlot{appliedOf(first.slot)}
	// A strongly consistent listing names every slot created before it began.
	listed := store.now()
	err := replay(ctx, store.objects, next, replayOptions{
		worldID: store.worldID, workers: store.shardWorkers,
		onSlot: func(slot *slotObject) { applied = append(applied, appliedOf(slot)) },
	})
	if _, broken := errors.AsType[*applyError](err); broken {
		return ahead{}, err
	}
	return ahead{next: next, applied: applied, confirmed: listed}, err
}

// reloadFlight is one reload that stale refreshes wait on.
type reloadFlight struct {
	done chan struct{}
	err  error
}

// reload waits, up to ctx, for a shared load from the newest checkpoint that
// outlives the request, since at scale it takes most of one request's time.
// After Close, when nothing runs in the background, the caller loads it.
func (store *Store) reload(ctx context.Context) error {
	store.reloadMu.Lock()
	flight := store.reloading
	if flight == nil {
		flight = &reloadFlight{done: make(chan struct{})}
		if !store.goBackground(func(life context.Context) { store.runReload(life, flight) }) {
			store.reloadMu.Unlock()
			return store.reloadIfBehind(ctx)
		}
		store.reloading = flight
	}
	store.reloadMu.Unlock()
	select {
	case <-flight.done:
		return flight.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (store *Store) runReload(life context.Context, flight *reloadFlight) {
	ctx, cancel := context.WithTimeout(life, checkpointTimeout)
	defer cancel()
	flight.err = store.reloadIfBehind(ctx)
	store.reloadMu.Lock()
	store.reloading = nil
	store.reloadMu.Unlock()
	close(flight.done)
}

// reloadIfBehind starts again from the newest checkpoint that loads when one
// lies past the served snapshot, or it diverged: an older release may have
// collected its next slot, which a probe takes for the tip and a commit forks.
func (store *Store) reloadIfBehind(ctx context.Context) error {
	served, diverged := store.served.Load().snap, store.diverged.Load()
	// All of them, since a fallback past a broken newer one may need one at or
	// below the served snapshot: a few, more while collection stalls, so each
	// attempt is bounded as at cold start.
	sequences, err := listCheckpoints(ctx, store.objects, 0)
	if err != nil || !diverged && lastOr(sequences, 0) <= served.Sequence {
		return err
	}
	started := store.now()
	base, err := store.loader(store.requestTimeout).newest(ctx, sequences)
	if err != nil {
		return err
	}
	store.logger.Warn("reloading from the newest checkpoint", "world", store.worldID, "served", served.Sequence, "checkpoint", base.snap.Sequence, "diverged", diverged)
	if err := store.start(ctx, base, started); err != nil {
		return err
	}
	store.diverged.Store(false)
	return nil
}

// install serves next, confirmed at the given time, and publishes the events
// of the slots that built it; installMu, which the caller holds, orders
// events and section steps in the log's order.
func (store *Store) install(next *snapshot, applied []appliedSlot, confirmed time.Time) {
	installed := store.rebased(next)
	store.served.Store(&served{snap: installed, confirmed: confirmed})
	store.releaseAdoption(installed)
	store.sections.installed(installed, applied)
	for _, slot := range applied {
		if slot.store != store.id {
			store.peerSlots.Add(1)
			store.commits.signal()
		}
		store.report(slot)
	}
	store.noteSlots(len(applied))
}

// derive starts a snapshot from s; see snapshot.derive.
func (store *Store) derive(s *snapshot) *snapshot {
	store.deriveMu.Lock()
	defer store.deriveMu.Unlock()
	return s.derive()
}

// poll refreshes the snapshot from the bucket, reporting what peers wrote.
func (store *Store) poll(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, store.requestTimeout)
	defer cancel()
	_, err := store.refresh(ctx)
	return err
}

// servedSequence is the sequence of the snapshot this replica serves.
func (store *Store) servedSequence() int64 { return store.served.Load().snap.Sequence }
