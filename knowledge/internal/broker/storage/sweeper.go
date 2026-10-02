package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Sweepable deletes its expired records and returns how many it removed;
// the refresh token and client registration stores implement it.
type Sweepable interface {
	Sweep(ctx context.Context) (int, error)
}

// SweptStore names a Sweepable for the logs.
type SweptStore struct {
	Name  string
	Store Sweepable
}

// Sweeper is the broker's periodic janitor: each tick sweeps every store.
type Sweeper struct {
	stores   []SweptStore
	interval time.Duration
	log      *slog.Logger
}

// NewSweeper builds a Sweeper over the stores to sweep; a nil log means
// slog.Default.
func NewSweeper(stores []SweptStore, interval time.Duration, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{stores: stores, interval: interval, log: log}
}

// Run sweeps once at start, so a new leader does not wait a full interval
// after a slow takeover, then every interval until ctx ends. Callers run it
// under leader.Run so one replica sweeps.
func (s *Sweeper) Run(ctx context.Context) {
	s.runOnce(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runOnce(ctx)
		}
	}
}

// runOnce logs a failed pass instead of stopping the loop.
func (s *Sweeper) runOnce(ctx context.Context) {
	if err := s.sweep(ctx); err != nil {
		s.log.ErrorContext(ctx, "broker: sweep failed", "err", err)
	}
}

// sweep is one pass over every store; one store's failure does not skip
// the others.
func (s *Sweeper) sweep(ctx context.Context) error {
	var errs []error
	for _, store := range s.stores {
		swept, err := store.Store.Sweep(ctx)
		if swept > 0 {
			s.log.InfoContext(ctx, "broker: swept expired records", "store", store.Name, "count", swept)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s sweep: %w", store.Name, err))
		}
	}
	return errors.Join(errs...)
}
