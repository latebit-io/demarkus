package blob

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// A call is hedged once it runs past the recent 95th percentile of its
	// kind, kept within these bounds; before enough samples, after the default.
	hedgeFloor   = 25 * time.Millisecond
	hedgeCeiling = time.Second
	hedgeDefault = 100 * time.Millisecond
	// hedgeSamples is the window the percentile is taken over, recomputed
	// every hedgeRecompute successes.
	hedgeSamples   = 256
	hedgeRecompute = 32
	// Creates keep a window per size class, so an upload is judged against
	// uploads of its size.
	smallCreate  = 64 << 10
	mediumCreate = 1 << 20
	// Each call earns a tenth of a hedge and at most hedgeBank are kept, so a
	// bucket that slows as a whole is sent at most about a tenth more calls.
	hedgeBank = 10
)

// Hedged sends a second Get or Create past the recent 95th percentile of its
// kind, keeping the first definitive answer. A create that finds its name taken
// once a hedge went out may have met itself, so it answers ErrAmbiguous.
func Hedged(store Store) Store {
	h := &hedged{Store: store}
	// Banked full, so a store's first calls may hedge before they earn any.
	h.budget.tenths.Store(hedgeBank * 10)
	return h
}

type hedged struct {
	Store
	gets    latencies
	creates [3]latencies // under smallCreate, under mediumCreate, larger
	budget  hedgeBudget
}

func (h *hedged) Get(ctx context.Context, key string) (Object, error) {
	return hedge(ctx, &h.gets, &h.budget, func(ctx context.Context) (Object, error) { return h.Store.Get(ctx, key) })
}

func (h *hedged) Create(ctx context.Context, key string, data []byte) (Attributes, error) {
	class := 0
	switch {
	case len(data) >= mediumCreate:
		class = 2
	case len(data) >= smallCreate:
		class = 1
	}
	return hedge(ctx, &h.creates[class], &h.budget, func(ctx context.Context) (Attributes, error) {
		return h.Store.Create(ctx, key, data)
	})
}

type hedgeAnswer[T any] struct {
	value T
	err   error
}

// hedge runs call, again past the delay when the budget allows; a success,
// not found or precondition wins at once, other errors wait for an attempt
// still running. A winning hedge's latency bounds the primary's from below.
func hedge[T any](ctx context.Context, observed *latencies, budget *hedgeBudget, call func(context.Context) (T, error)) (T, error) {
	budget.earn()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	answers := make(chan hedgeAnswer[T], 2)
	attempt := func() {
		value, err := call(ctx)
		answers <- hedgeAnswer[T]{value: value, err: err}
	}
	started := time.Now()
	go attempt()
	timer := time.NewTimer(observed.delay())
	defer timer.Stop()
	running, hedgeSent := 1, false
	for {
		select {
		case <-timer.C:
			if budget.spend() {
				running, hedgeSent = running+1, true
				go attempt()
			}
		case answer := <-answers:
			running--
			switch {
			case answer.err == nil:
				observed.add(time.Since(started))
				return answer.value, nil
			case hedgeSent && errors.Is(answer.err, ErrPrecondition):
				var zero T
				return zero, fmt.Errorf("%w: a hedged attempt found the name taken: %v", ErrAmbiguous, answer.err)
			case errors.Is(answer.err, ErrNotFound) || errors.Is(answer.err, ErrPrecondition) || running == 0:
				// A primary that fails before the delay is the caller's to retry.
				return answer.value, answer.err
			}
		}
	}
}

// hedgeBudget counts tenths of a hedge.
type hedgeBudget struct{ tenths atomic.Int64 }

func (b *hedgeBudget) earn() {
	for {
		tenths := b.tenths.Load()
		if tenths >= hedgeBank*10 || b.tenths.CompareAndSwap(tenths, tenths+1) {
			return
		}
	}
}

func (b *hedgeBudget) spend() bool {
	for {
		tenths := b.tenths.Load()
		if tenths < 10 {
			return false
		}
		if b.tenths.CompareAndSwap(tenths, tenths-10) {
			return true
		}
	}
}

// latencies keeps a window of recent successful latencies and its 95th
// percentile, which callers read without the lock.
type latencies struct {
	mu      sync.Mutex
	samples [hedgeSamples]time.Duration
	count   int
	p95     atomic.Int64 // zero until the first recompute
}

func (l *latencies) add(latency time.Duration) {
	l.mu.Lock()
	l.samples[l.count%hedgeSamples] = latency
	l.count++
	if l.count%hedgeRecompute != 0 {
		l.mu.Unlock()
		return
	}
	window, size := l.samples, min(l.count, hedgeSamples)
	l.mu.Unlock()
	sorted := window[:size]
	slices.Sort(sorted)
	l.p95.Store(int64(sorted[size*95/100]))
}

func (l *latencies) delay() time.Duration {
	p95 := time.Duration(l.p95.Load())
	if p95 == 0 {
		return hedgeDefault
	}
	return min(max(p95, hedgeFloor), hedgeCeiling)
}
