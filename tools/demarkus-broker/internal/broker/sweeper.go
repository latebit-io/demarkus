package broker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Sweeper is the broker's periodic janitor. Each tick it retires
// expired refresh tokens from the broker's refresh-tokens Secret so the
// store doesn't accumulate dead grants. The broker no longer mints
// per-user demarkus tokens, so there is no issuance state to sweep —
// world write tokens are long-lived and provisioned once per world.
//
// Across broker replicas, RunLeaderElected ensures only the holder of a
// coordination.k8s.io/Lease runs the sweep loop. The other replicas
// keep serving requests (those don't need a leader).
type Sweeper struct {
	k8s          kubernetes.Interface
	refreshStore *RefreshStore
	interval     time.Duration
	log          *slog.Logger
	// sweepHook fires after each sweep pass when non-nil. Test-only
	// observability point so the leader-election test can count which
	// replica's loop is running without scraping logs or polling the
	// Lease object. Production code never sets it.
	sweepHook func()
}

// NewSweeper builds a Sweeper. interval is the cadence between sweep
// passes when running (5 min default from config); leader-election
// timings live in RunLeaderElected. A nil logger falls back to slog
// default so callers building one for ad-hoc tests don't have to plumb
// a handler through.
//
// k8s is used for the leader-election Lease lock. refreshStore is the
// Secret-backed store whose expired grants the loop retires; passing
// nil makes the sweep a no-op (used by tests that exercise only the
// lifecycle plumbing). Production wiring passes the same *RefreshStore
// the Server holds so both share a consistent view of the Secret.
func NewSweeper(k8s kubernetes.Interface, refreshStore *RefreshStore, interval time.Duration, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{k8s: k8s, refreshStore: refreshStore, interval: interval, log: log}
}

// RunLeaderElected blocks until ctx is canceled, running the sweep loop
// only while this replica holds the named Lease in namespace. Multi-
// replica deployments must pass the same leaseName + namespace so all
// replicas race for the one Lease. identity is the holder field that
// shows up in the Lease object (read from POD_NAME with a hostname
// fallback in production wiring).
//
// LeaseDuration/RenewDeadline/RetryPeriod use the client-go defaults
// (15s/10s/2s) — failover budget is roughly the LeaseDuration. Lease
// is released on graceful context cancel so the successor takes over
// in milliseconds rather than waiting out the expiry.
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

// Run starts the sweep loop directly, with no leader election: for
// single-host (file-backend) deployments, which are single-instance by
// construction. Multi-replica k8s deployments use RunLeaderElected.
func (s *Sweeper) Run(ctx context.Context) {
	s.runLoop(ctx)
}

// runLoop runs an initial sweep then ticks every Sweeper.interval until
// ctx is canceled. The eager first pass means a freshly-elected leader
// doesn't have to wait a full interval before pruning expired grants
// from a slow takeover — important when LeaseDuration > Interval on
// rollouts. Test entry point: tests drive runLoop directly without
// leader-election plumbing.
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

// runOnce is the per-tick boundary: swallow + log sweep errors so the
// loop survives transient k8s API blips.
func (s *Sweeper) runOnce(ctx context.Context) {
	if err := s.sweep(ctx); err != nil {
		s.log.ErrorContext(ctx, "broker: sweep failed", "err", err)
	}
	if s.sweepHook != nil {
		s.sweepHook()
	}
}

// sweep performs one cleanup pass: retire expired refresh tokens. A nil
// refreshStore makes this a no-op (lifecycle-only test sweepers).
func (s *Sweeper) sweep(ctx context.Context) error {
	if s.refreshStore == nil {
		return nil
	}
	swept, err := s.refreshStore.Sweep(ctx)
	if err != nil {
		return fmt.Errorf("refresh token sweep: %w", err)
	}
	if swept > 0 {
		s.log.InfoContext(ctx, "broker: swept refresh tokens", "count", swept)
	}
	return nil
}
