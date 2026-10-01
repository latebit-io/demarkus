package storage

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubSweeper counts passes and returns a scripted result.
type stubSweeper struct {
	calls atomic.Int32
	n     int
	err   error
}

func (s *stubSweeper) Sweep(context.Context) (int, error) {
	s.calls.Add(1)
	return s.n, s.err
}

func TestSweepCallsEveryStore(t *testing.T) {
	// Each pass calls every store once; one failure is surfaced named and
	// does not skip the next store. Which records expire is each store's
	// contract (refresh_test.go, dynamic_clients_test.go).
	refresh, clients := &stubSweeper{n: 2}, &stubSweeper{n: 1}
	s := NewSweeper([]SweptStore{{Name: "refresh", Store: refresh}, {Name: "clients", Store: clients}}, time.Hour, nil)
	if err := s.sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if refresh.calls.Load() != 1 || clients.calls.Load() != 1 {
		t.Errorf("Sweep calls = %d, %d; want 1, 1", refresh.calls.Load(), clients.calls.Load())
	}
	refresh.err = errors.New("bucket unreachable")
	err := s.sweep(context.Background())
	if err == nil || !errors.Is(err, refresh.err) || !strings.Contains(err.Error(), "refresh sweep") {
		t.Errorf("sweep err = %v, want the named store error", err)
	}
	if clients.calls.Load() != 2 {
		t.Errorf("clients Sweep calls = %d after a refresh failure, want 2", clients.calls.Load())
	}
}

func TestSweeperRunSweepsAtStartAndEachInterval(t *testing.T) {
	// Leadership is leader.Run's contract; Run here is the loop it leads.
	store := &stubSweeper{}
	s := NewSweeper([]SweptStore{{Name: "stub", Store: store}}, 20*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	waitFor(t, func() bool { return store.calls.Load() >= 3 })
	cancel()
	<-done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
