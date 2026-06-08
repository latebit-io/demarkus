package broker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func TestSweepRemovesExpiredRefreshTokens(t *testing.T) {
	// When the Sweeper is wired with a *RefreshStore, its per-tick pass
	// removes expired refresh-token records from the broker-namespace
	// Secret. The broker no longer mints per-user demarkus tokens, so
	// refresh tokens are the only thing the sweeper retires.
	k8s := fake.NewSimpleClientset()
	cfg := testRefreshConfig()
	rs := NewRefreshStore(cfg, k8s)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	rs.clock = func() time.Time { return base }

	short, err := rs.Issue(context.Background(), &Claims{Email: "short@x.com", EmailVerified: true}, time.Hour)
	if err != nil {
		t.Fatalf("Issue short: %v", err)
	}
	long, err := rs.Issue(context.Background(), &Claims{Email: "long@x.com", EmailVerified: true}, 48*time.Hour)
	if err != nil {
		t.Fatalf("Issue long: %v", err)
	}

	// Advance the store's clock past the short token's ExpiresAt
	// but not the long one's.
	rs.clock = func() time.Time { return base.Add(2 * time.Hour) }

	s := NewSweeper(k8s, rs, time.Hour, nil)
	if err := s.sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := rs.Refresh(context.Background(), short); err == nil {
		t.Errorf("expired token survived sweep")
	}
	if _, err := rs.Refresh(context.Background(), long); err != nil {
		t.Errorf("long-lived token swept by accident: %v", err)
	}
}

func TestSweepNoRefreshStoreIsNoop(t *testing.T) {
	// A Sweeper constructed without a RefreshStore (lifecycle-only
	// sweepers in tests) must not panic when its per-tick loop reaches
	// the refresh-sweep block. Exercises the nil-skip.
	k8s := fake.NewSimpleClientset()
	s := NewSweeper(k8s, nil, time.Hour, nil)
	if err := s.sweep(context.Background()); err != nil {
		t.Fatalf("sweep with nil refreshStore: %v", err)
	}
}

func TestSweeperLeaderElection(t *testing.T) {
	// Two Sweeper instances race for the same Lease in the same
	// namespace against a shared fake clientset. Exactly one is
	// elected and runs its loop; the follower stays dormant. Cancel
	// the leader's context — ReleaseOnCancel hands the Lease back, the
	// follower picks it up and starts sweeping. Validates
	// RunLeaderElected as a singleton coordinator across replicas.
	if testing.Short() {
		t.Skip("skipping leader-election test in short mode")
	}
	k8s := fake.NewSimpleClientset()
	// Two sweepers backed by the same fake clientset so leader
	// election state is shared. Each replica gets its own Sweeper
	// with its own counter.
	makeReplica := func(identity string) (*Sweeper, *atomic.Int32) {
		var count atomic.Int32
		s := NewSweeper(k8s, nil, 30*time.Millisecond, nil)
		s.sweepHook = func() { count.Add(1) }
		_ = identity // wired into RunLeaderElected below
		return s, &count
	}
	sweepA, countA := makeReplica("A")
	sweepB, countB := makeReplica("B")

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	doneA := make(chan struct{})
	doneB := make(chan struct{})
	go func() {
		defer close(doneA)
		sweepA.RunLeaderElected(ctxA, "test-sweeper-lease", testBrokerNS, "A")
	}()
	go func() {
		defer close(doneB)
		sweepB.RunLeaderElected(ctxB, "test-sweeper-lease", testBrokerNS, "B")
	}()

	// Wait for exactly one to start sweeping.
	leaderIsA := waitFor(t, 10*time.Second, func() bool {
		return countA.Load() > 0 || countB.Load() > 0
	}) && countA.Load() > 0
	leaderIsB := countB.Load() > 0

	if leaderIsA == leaderIsB {
		t.Fatalf("both or neither swept: A=%d B=%d", countA.Load(), countB.Load())
	}

	// Hold for a few more ticks; the follower must remain at zero.
	var (
		leaderCount, followerCount   *atomic.Int32
		cancelLeader, cancelFollower context.CancelFunc
		doneLeader, doneFollower     chan struct{}
	)
	if leaderIsA {
		leaderCount, followerCount = countA, countB
		cancelLeader, cancelFollower = cancelA, cancelB
		doneLeader, doneFollower = doneA, doneB
	} else {
		leaderCount, followerCount = countB, countA
		cancelLeader, cancelFollower = cancelB, cancelA
		doneLeader, doneFollower = doneB, doneA
	}

	time.Sleep(200 * time.Millisecond)
	if followerCount.Load() != 0 {
		t.Errorf("follower swept while leader held lease: leader=%d follower=%d",
			leaderCount.Load(), followerCount.Load())
	}

	// Cancel the leader. Its loop must unwind, releasing the Lease,
	// and the follower must take over within a few hundred ms.
	cancelLeader()
	<-doneLeader
	followerBaseline := followerCount.Load()
	waitFor(t, 10*time.Second, func() bool {
		return followerCount.Load() > followerBaseline
	})

	// Cancel and wait for the follower too. The deferred cancels at
	// the top of the test would eventually fire, but they fire on
	// return — without an explicit join here the follower's renewal
	// loop and the test's cleanup race, leaving the goroutine and
	// fake-clientset Lease state to leak into the next test.
	cancelFollower()
	<-doneFollower
}

// waitFor polls cond every 10ms until it returns true or the timeout
// elapses. Returns true on success, fails the test on timeout. Used by
// the leader-election test to assert "eventually" conditions without
// resorting to fixed sleeps that either over- or under-shoot.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
	return false
}
