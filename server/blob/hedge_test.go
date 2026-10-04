package blob

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// warmed is a window whose percentile sits under the floor, so calls hedge
// after hedgeFloor.
func warmed() *latencies {
	observed := &latencies{}
	warm(observed)
	return observed
}

// funded is a budget with every hedge banked.
func funded() *hedgeBudget {
	budget := &hedgeBudget{}
	budget.tenths.Store(hedgeBank * 10)
	return budget
}

func warm(observed *latencies) {
	for range hedgeRecompute {
		observed.add(time.Millisecond)
	}
}

func TestHedge(t *testing.T) {
	t.Run("a slow call is hedged and the first answer wins", func(t *testing.T) {
		var calls atomic.Int64
		primaryCanceled := make(chan struct{})
		value, err := hedge(context.Background(), warmed(), funded(), func(ctx context.Context) (string, error) {
			if calls.Add(1) == 1 {
				<-ctx.Done()
				close(primaryCanceled)
				return "", ctx.Err()
			}
			return "hedge", nil
		})
		if err != nil || value != "hedge" {
			t.Fatalf("hedged call = %q, %v; want the hedge's answer", value, err)
		}
		select {
		case <-primaryCanceled:
		case <-time.After(time.Second):
			t.Fatal("the slow primary was not canceled")
		}
	})

	t.Run("a fast call is not hedged", func(t *testing.T) {
		var calls atomic.Int64
		if _, err := hedge(context.Background(), warmed(), funded(), func(context.Context) (int, error) { return int(calls.Add(1)), nil }); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * hedgeFloor)
		if n := calls.Load(); n != 1 {
			t.Fatalf("calls = %d, want 1", n)
		}
	})

	t.Run("a primary that fails at once is the caller's to retry", func(t *testing.T) {
		var calls atomic.Int64
		_, err := hedge(context.Background(), warmed(), funded(), func(context.Context) (int, error) {
			calls.Add(1)
			return 0, ErrUnavailable
		})
		if !errors.Is(err, ErrUnavailable) || calls.Load() != 1 {
			t.Fatalf("hedge = %v after %d calls, want the primary's error from one call", err, calls.Load())
		}
	})

	t.Run("an error waits for the attempt still running", func(t *testing.T) {
		var calls atomic.Int64
		hedgeStarted := make(chan struct{})
		value, err := hedge(context.Background(), warmed(), funded(), func(context.Context) (string, error) {
			if calls.Add(1) == 1 {
				<-hedgeStarted
				return "", ErrUnavailable
			}
			close(hedgeStarted)
			time.Sleep(10 * time.Millisecond)
			return "hedge", nil
		})
		if err != nil || value != "hedge" {
			t.Fatalf("hedge = %q, %v; want the running attempt's success", value, err)
		}
	})

	t.Run("without budget a slow call runs alone", func(t *testing.T) {
		var calls atomic.Int64
		_, err := hedge(context.Background(), warmed(), &hedgeBudget{}, func(context.Context) (int, error) {
			calls.Add(1)
			time.Sleep(3 * hedgeFloor)
			return 0, nil
		})
		if err != nil || calls.Load() != 1 {
			t.Fatalf("hedge = %v after %d calls, want one call without budget", err, calls.Load())
		}
	})

	t.Run("the budget allows about one hedge in ten calls", func(t *testing.T) {
		budget := &hedgeBudget{}
		hedges := 0
		for range 100 {
			budget.earn()
			if budget.spend() {
				hedges++
			}
		}
		if hedges != 10 {
			t.Fatalf("hedges allowed over 100 calls = %d, want 10", hedges)
		}
	})

	t.Run("a definitive answer wins at once", func(t *testing.T) {
		var calls atomic.Int64
		_, err := hedge(context.Background(), warmed(), funded(), func(ctx context.Context) (int, error) {
			if calls.Add(1) == 1 {
				<-ctx.Done()
				return 0, ctx.Err()
			}
			return 0, ErrNotFound
		})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("hedge = %v, want the hedge's not found", err)
		}
	})
}

// A create whose own other attempt took the name answers ErrAmbiguous, not
// ErrPrecondition: the name may hold its bytes, so the caller reads it back.
func TestHedgedCreateMeetsItself(t *testing.T) {
	memory, err := NewMemory(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	store := &hedged{Store: &answersLate{Store: memory}}
	warm(&store.creates[0])
	store.budget.tenths.Store(hedgeBank * 10)
	_, err = store.Create(context.Background(), "k", []byte("v"))
	if !errors.Is(err, ErrAmbiguous) || errors.Is(err, ErrPrecondition) {
		t.Fatalf("create beaten by its own attempt = %v, want ambiguous only", err)
	}
	if object, err := memory.Get(context.Background(), "k"); err != nil || string(object.Data) != "v" {
		t.Fatalf("name holds %q, %v; want the create's bytes", object.Data, err)
	}
}

// answersLate lands its first create at once and answers only when the call's
// context ends.
type answersLate struct {
	Store
	calls atomic.Int64
}

func (s *answersLate) Create(ctx context.Context, key string, data []byte) (Attributes, error) {
	attributes, err := s.Store.Create(ctx, key, data)
	if s.calls.Add(1) == 1 {
		<-ctx.Done()
	}
	return attributes, err
}

func TestHedgeDelay(t *testing.T) {
	observed := &latencies{}
	if got := observed.delay(); got != hedgeDefault {
		t.Fatalf("delay before samples = %v, want %v", got, hedgeDefault)
	}
	for index := range hedgeSamples {
		observed.add(time.Duration(index+1) * time.Millisecond)
	}
	if got := observed.delay(); got < 240*time.Millisecond || got > 245*time.Millisecond {
		t.Fatalf("delay = %v, want the window's 95th percentile", got)
	}
	for range hedgeSamples {
		observed.add(time.Hour)
	}
	if got := observed.delay(); got != hedgeCeiling {
		t.Fatalf("delay = %v, want the ceiling", got)
	}
}

// A store's first calls may hedge before they have earned any budget, so a
// new world's first stalled read does not wait out its deadline.
func TestHedgedStartsFunded(t *testing.T) {
	memory, err := NewMemory(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Create(context.Background(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	store := Hedged(&stallsFirstGet{Store: memory})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	if object, err := store.Get(ctx, "k"); err != nil || string(object.Data) != "v" {
		t.Fatalf("get = %q, %v; want the hedge's answer", object.Data, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("first stalled get took %v, want it hedged after %v", elapsed, hedgeDefault)
	}
}

// stallsFirstGet holds its first Get until the call's context ends.
type stallsFirstGet struct {
	Store
	calls atomic.Int64
}

func (s *stallsFirstGet) Get(ctx context.Context, key string) (Object, error) {
	if s.calls.Add(1) == 1 {
		<-ctx.Done()
		return Object{}, ctx.Err()
	}
	return s.Store.Get(ctx, key)
}
