package broker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/latebit/demarkus/protocol/token"
)

// Sweeper is the broker's periodic janitor. Each tick it walks the
// issuances Secret and retires entries that fall into either bucket:
//
//   - Expired: now > issuance.ExpiresAt. The primary revocation
//     mechanism for short-lived tokens per the plan §Revocation §2.
//   - Drifted: the issuance record points at a label that's no longer
//     in the world's tokens Secret. Causes: an operator hand-edited
//     the world Secret, or a prior Revoke failed midway between the
//     two-Secret writes leaving an orphan in issuances. Pruning
//     these keeps List/Revoke and the issuances-Secret-as-index
//     consistent with the world Secret as source of truth.
//
// One pass per tick: a single read of the issuances Secret, grouped by
// world; each affected world's tokens Secret is read at most once for
// drift detection. Concurrent mints and revokes on the same Secrets are
// handled by mutateSecret's optimistic-concurrency retry — no extra
// locking is needed here.
//
// Across broker replicas, RunLeaderElected ensures only the holder of a
// coordination.k8s.io/Lease runs the sweep loop. The other replicas
// keep serving Mint/List/Revoke (those don't need a leader).
type Sweeper struct {
	issuer   *Issuer
	interval time.Duration
	clock    func() time.Time
	log      *slog.Logger
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
func NewSweeper(i *Issuer, interval time.Duration, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{issuer: i, interval: interval, clock: time.Now, log: log}
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
		Client:     s.issuer.k8s.CoordinationV1(),
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

// runLoop runs an initial sweep then ticks every Sweeper.interval until
// ctx is canceled. The eager first pass means a freshly-elected leader
// doesn't have to wait a full interval before pruning expired entries
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
// loop survives transient k8s API blips. Per-entry failures inside
// sweep are also non-fatal (logged and skipped) so one stuck label
// can't starve the rest.
func (s *Sweeper) runOnce(ctx context.Context) {
	if err := s.sweep(ctx); err != nil {
		s.log.ErrorContext(ctx, "broker: sweep failed", "err", err)
	}
	if s.sweepHook != nil {
		s.sweepHook()
	}
}

// sweep performs one cleanup pass. Returns an error only when the
// top-level issuances read fails — every other failure is logged in
// place and the loop continues with the remaining entries.
//
// Memory: the full issuances list is loaded into memory each tick.
// The implicit upper bound is the Kubernetes Secret size limit (~1MB
// after etcd encoding), which works out to roughly 5000–7000
// issuance records at ~150 bytes apiece. Long before sweep memory
// becomes a problem, the issuances Secret itself stops being
// writable — which is the actual scaling wall for this design.
// Pagination would address neither failure mode (Secrets aren't a
// paginated resource), so the right Phase-7 fix is a different
// backing store (CRD, namespaced ConfigMap-per-user, etc.), not a
// streaming sweep.
func (s *Sweeper) sweep(ctx context.Context) error {
	all, err := s.issuer.readIssuances(ctx)
	if err != nil {
		return fmt.Errorf("read issuances: %w", err)
	}
	if len(all.Entries) == 0 {
		return nil
	}
	now := s.clock()

	// Two-pass collection so drift detection only fetches a world's
	// tokens Secret once even when multiple issuances point at it.
	// Expired entries skip the drift check — they're getting retired
	// either way and revokeIssuance is idempotent if the label is
	// already absent from the world Secret.
	var toRevoke []Issuance
	perWorld := make(map[string][]Issuance)
	for j := range all.Entries {
		iss := all.Entries[j]
		if !iss.ExpiresAt.IsZero() && now.After(iss.ExpiresAt) {
			toRevoke = append(toRevoke, iss)
			continue
		}
		perWorld[iss.World] = append(perWorld[iss.World], iss)
	}

	for worldName, isses := range perWorld {
		world := s.issuer.lookupWorld(worldName)
		if world == nil {
			// Same conservative posture as Revoke: don't silently
			// drop issuance records for a world the broker is no
			// longer configured for. The operator either intended
			// to remove the world (and needs to clean up issuances
			// explicitly) or the config is in flight; either way,
			// we surface and skip.
			s.log.WarnContext(ctx, "broker: sweep skipped issuances for unconfigured world",
				"world", worldName, "count", len(isses))
			continue
		}
		labels, err := s.readWorldLabels(ctx, world)
		if err != nil {
			s.log.ErrorContext(ctx, "broker: sweep read world tokens secret failed",
				"world", worldName, "err", err)
			continue
		}
		for j := range isses {
			if _, present := labels[isses[j].Label]; !present {
				toRevoke = append(toRevoke, isses[j])
			}
		}
	}

	for j := range toRevoke {
		iss := &toRevoke[j]
		reason := "drift"
		if !iss.ExpiresAt.IsZero() && now.After(iss.ExpiresAt) {
			reason = "expired"
		}
		if err := s.issuer.revokeIssuance(ctx, iss); err != nil {
			s.log.ErrorContext(ctx, "broker: sweep revoke failed",
				"label", iss.Label, "world", iss.World, "reason", reason, "err", err)
			continue
		}
		s.log.InfoContext(ctx, "broker: swept",
			"label", iss.Label, "world", iss.World,
			"subject", hashSubject(iss.Email), "reason", reason)
	}
	return nil
}

// readWorldLabels fetches a world's tokens Secret and returns its label
// set. A missing Secret yields an empty (non-nil) set; from drift's
// point of view that's "no labels present" — every issuance against
// that world has effectively drifted (probably operator
// pre-deployment state, but the right thing is still to prune the
// stale issuances).
func (s *Sweeper) readWorldLabels(ctx context.Context, w *WorldConfig) (map[string]struct{}, error) {
	secret, err := s.issuer.k8s.CoreV1().Secrets(w.Namespace).Get(ctx, w.TokensSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get tokens secret %s/%s: %w", w.Namespace, w.TokensSecret, err)
	}
	f, err := token.ParseBytes(secret.Data[TokensSecretKey])
	if err != nil {
		return nil, fmt.Errorf("parse tokens secret %s/%s: %w", w.Namespace, w.TokensSecret, err)
	}
	labels := make(map[string]struct{}, len(f.Tokens))
	for label := range f.Tokens {
		labels[label] = struct{}{}
	}
	return labels, nil
}
