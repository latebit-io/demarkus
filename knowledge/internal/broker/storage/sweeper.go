package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
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
// RunLeaderElected keeps one replica sweeping through a Lease; the others
// keep serving.
type Sweeper struct {
	k8s      kubernetes.Interface
	stores   []SweptStore
	interval time.Duration
	log      *slog.Logger
	// sweepHook fires after each pass; tests count a replica's ticks with it.
	sweepHook func()
}

// NewSweeper builds a Sweeper over the Lease client and the stores to sweep;
// a nil log means slog.Default.
func NewSweeper(k8s kubernetes.Interface, stores []SweptStore, interval time.Duration, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{k8s: k8s, stores: stores, interval: interval, log: log}
}

// RunLeaderElected sweeps only while this replica holds the Lease; every
// replica passes the same leaseName and namespace. The client-go default
// timings bound failover by LeaseDuration; cancel releases the Lease at once.
func (s *Sweeper) RunLeaderElected(ctx context.Context, leaseName, namespace, identity string) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: leaseName, Namespace: namespace},
		Client:     s.k8s.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: s.runLoop,
			OnStoppedLeading: func() {
				s.log.Info("broker: sweeper lost leadership", "identity", identity)
			},
			OnNewLeader: func(id string) {
				if id != identity {
					s.log.Info("broker: sweeper observing new leader", "leader", id, "self", identity)
				}
			},
		},
	})
}

// runLoop sweeps once at start, so a new leader does not wait a full
// interval after a slow takeover, then every interval until ctx ends.
func (s *Sweeper) runLoop(ctx context.Context) {
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
	if s.sweepHook != nil {
		s.sweepHook()
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
