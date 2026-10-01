package leader

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testLease = "test-lease"
	testNS    = "test-ns"
)

// fast keeps tests short. The Lease stores whole seconds, so a sub-second
// lease reads as already expired; renew must exceed retry times 1.2.
var fast = timing{lease: time.Second, renew: 500 * time.Millisecond, retry: 100 * time.Millisecond}

func election(leases coordinationv1.LeasesGetter, identity string) Election {
	return Election{Leases: leases, LeaseName: testLease, Namespace: testNS, Identity: identity, timing: fast}
}

// block is a lead that holds its term until the term ends.
func block(leadCtx context.Context) { <-leadCtx.Done() }

// replica runs one contender in the background; terms counts its leads.
type replica struct {
	terms  atomic.Int32
	leader atomic.Bool
	cancel context.CancelFunc
	done   chan struct{} // closed once Run returns
	err    error         // Run's result, set before done closes
}

func start(t *testing.T, e Election, lead func(context.Context)) *replica {
	t.Helper()
	r := &replica{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() {
		defer close(r.done)
		r.err = Run(ctx, e, func(leadCtx context.Context) {
			r.terms.Add(1)
			r.leader.Store(true)
			defer r.leader.Store(false)
			lead(leadCtx)
		})
	}()
	t.Cleanup(func() { r.stop(t) })
	return r
}

// stop cancels the replica and joins Run, so no renewal outlives the test.
func (r *replica) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case <-r.done:
		if r.err != nil {
			t.Errorf("Run: %v", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestOneLeaderThenHandover(t *testing.T) {
	leases := fake.NewSimpleClientset().CoordinationV1()
	a := start(t, election(leases, "A"), block)
	b := start(t, election(leases, "B"), block)
	waitFor(t, func() bool { return a.leader.Load() || b.leader.Load() })
	time.Sleep(3 * fast.retry)
	if a.leader.Load() == b.leader.Load() {
		t.Fatalf("leaders: A=%v B=%v, want exactly one", a.leader.Load(), b.leader.Load())
	}
	held, follower := a, b
	if b.leader.Load() {
		held, follower = b, a
	}
	held.stop(t)
	waitFor(t, follower.leader.Load)
}

func TestContendsAgainAfterLostTerm(t *testing.T) {
	// client-go's Run returns for good once a renewal fails; Run must not.
	client := fake.NewSimpleClientset()
	var partitioned atomic.Bool
	client.PrependReactor("*", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if partitioned.Load() {
			return true, nil, errors.New("apiserver unreachable")
		}
		return false, nil, nil
	})
	a := start(t, election(client.CoordinationV1(), "A"), block)
	waitFor(t, a.leader.Load)
	partitioned.Store(true)
	waitFor(t, func() bool { return !a.leader.Load() })
	partitioned.Store(false)
	waitFor(t, func() bool { return a.terms.Load() >= 2 })
}

func TestLeadReturningEarlyReleasesTheLease(t *testing.T) {
	// A lead that returns while leading must not keep a dead Lease.
	leases := fake.NewSimpleClientset().CoordinationV1()
	a := start(t, election(leases, "A"), func(context.Context) {})
	waitFor(t, func() bool { return a.terms.Load() >= 2 })
}

func TestRunReturnsAfterLead(t *testing.T) {
	leases := fake.NewSimpleClientset().CoordinationV1()
	var finished atomic.Bool
	a := start(t, election(leases, "A"), func(leadCtx context.Context) {
		<-leadCtx.Done()
		time.Sleep(50 * time.Millisecond)
		finished.Store(true)
	})
	waitFor(t, a.leader.Load)
	a.stop(t)
	if !finished.Load() {
		t.Error("Run returned before lead finished")
	}
}

func TestEmptyIdentityIsAnError(t *testing.T) {
	e := election(fake.NewSimpleClientset().CoordinationV1(), "")
	err := Run(context.Background(), e, func(context.Context) { t.Error("lead ran without an identity") })
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("Run err = %v, want the empty identity error", err)
	}
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
