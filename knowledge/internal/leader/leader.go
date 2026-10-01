// Package leader runs work on one replica at a time through a Kubernetes
// Lease, and contends again after a lost term until shutdown.
package leader

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Election is one Lease that every replica of a role contends for.
type Election struct {
	Leases    coordinationv1.LeasesGetter
	LeaseName string
	Namespace string
	Identity  string       // this replica's holder name; must not be empty
	Log       *slog.Logger // nil means slog.Default
	timing    timing       // zero means defaultTiming; tests shorten it
}

type timing struct {
	lease, renew, retry time.Duration
}

// Failover is bounded by the lease duration; a cancelled term releases at once.
var defaultTiming = timing{lease: 15 * time.Second, renew: 10 * time.Second, retry: 2 * time.Second}

// Run calls lead only while this replica holds the Lease; lead returns once its
// ctx ends. A lost term contends again, so a failed renewal never retires a
// replica. Run returns after lead, when ctx ends or on a config error.
func Run(ctx context.Context, e Election, lead func(context.Context)) error { //nolint:gocritic // an election is built once and passed once
	e.Log = cmp.Or(e.Log, slog.Default()).With("lease", e.Namespace+"/"+e.LeaseName, "identity", e.Identity)
	if e.timing == (timing{}) {
		e.timing = defaultTiming
	}
	e.Log.Info("leader: contending")
	for {
		if err := e.term(ctx, lead); err != nil {
			return err
		}
		// Bounds Lease churn when lead returns at once.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(e.timing.retry):
		}
	}
}

// term contends once and leads until the term ends. lead runs on this
// goroutine because client-go does not wait for OnStartedLeading: terms on
// one replica never overlap, and Run never outlives lead.
func (e *Election) term(ctx context.Context, lead func(context.Context)) error {
	termCtx, endTerm := context.WithCancel(ctx)
	defer endTerm()
	started := make(chan context.Context, 1)
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Name: e.LeaseName, Namespace: e.Namespace},
			Client:     e.Leases,
			LockConfig: resourcelock.ResourceLockConfig{Identity: e.Identity},
		},
		Name:            e.LeaseName,
		LeaseDuration:   e.timing.lease,
		RenewDeadline:   e.timing.renew,
		RetryPeriod:     e.timing.retry,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leadCtx context.Context) { started <- leadCtx },
			OnStoppedLeading: func() {},
			OnNewLeader: func(id string) {
				if id != e.Identity {
					e.Log.Info("leader: observing leader", "leader", id)
				}
			},
		},
	})
	if err != nil {
		return fmt.Errorf("leader election %s/%s: %w", e.Namespace, e.LeaseName, err)
	}
	electorDone := make(chan struct{})
	go func() {
		defer close(electorDone)
		elector.Run(termCtx)
	}()
	select {
	case leadCtx := <-started:
		e.Log.Info("leader: started leading")
		lead(leadCtx)
		if leadCtx.Err() == nil {
			e.Log.Warn("leader: lead returned while leading; releasing the Lease")
		}
		endTerm()
		<-electorDone
		if ctx.Err() == nil {
			e.Log.Warn("leader: term ended; contending again")
		}
	case <-electorDone:
	}
	return nil
}
