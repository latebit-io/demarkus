package broker

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/latebit/demarkus/protocol/token"
)

// seedIssuance writes a single issuance record into the broker
// namespace alongside an entry in the world's tokens Secret. Bypasses
// Mint so individual sweeper tests can manipulate ExpiresAt directly.
// Multi-call: subsequent calls against the same world append to the
// existing TOML rather than replacing it.
func seedIssuance(t *testing.T, k8s *fake.Clientset, iss *Issuance) {
	t.Helper()
	world := worldByName(t, iss.World)
	tokensSecret, getErr := k8s.CoreV1().Secrets(world.Namespace).Get(context.Background(), world.TokensSecret, metav1.GetOptions{})
	notFound := apierrors.IsNotFound(getErr)
	if !notFound && getErr != nil {
		t.Fatalf("get world tokens secret: %v", getErr)
	}
	if notFound {
		tokensSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: world.TokensSecret, Namespace: world.Namespace},
			Data:       map[string][]byte{},
		}
	}
	updated, err := token.AppendBytes(tokensSecret.Data[TokensSecretKey], iss.Label, &token.Entry{
		Hash:       "sha256-seeded",
		Paths:      iss.Paths,
		Operations: iss.Operations,
		Expires:    iss.ExpiresAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("seed AppendBytes: %v", err)
	}
	if tokensSecret.Data == nil {
		tokensSecret.Data = map[string][]byte{}
	}
	tokensSecret.Data[TokensSecretKey] = updated
	if notFound {
		if _, err := k8s.CoreV1().Secrets(world.Namespace).Create(context.Background(), tokensSecret, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create world tokens secret: %v", err)
		}
	} else {
		if _, err := k8s.CoreV1().Secrets(world.Namespace).Update(context.Background(), tokensSecret, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("update world tokens secret: %v", err)
		}
	}
	seedIssuanceRecord(t, k8s, iss)
}

// seedIssuanceRecord adds only the broker-side issuance entry without
// touching any world Secret. Used by drift tests to simulate the
// orphan-in-issuances state (label present in broker state, absent
// from world Secret).
func seedIssuanceRecord(t *testing.T, k8s *fake.Clientset, iss *Issuance) {
	t.Helper()
	brokerSecret, getErr := k8s.CoreV1().Secrets(testBrokerNS).Get(context.Background(), testIssuancesNS, metav1.GetOptions{})
	notFound := apierrors.IsNotFound(getErr)
	if !notFound && getErr != nil {
		t.Fatalf("get issuances secret: %v", getErr)
	}
	var current Issuances
	if notFound {
		brokerSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testIssuancesNS, Namespace: testBrokerNS},
			Data:       map[string][]byte{},
		}
	} else if raw := brokerSecret.Data[IssuancesSecretKey]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &current); err != nil {
			t.Fatalf("decode issuances seed: %v", err)
		}
	}
	current.Entries = append(current.Entries, *iss)
	payload, err := json.Marshal(current)
	if err != nil {
		t.Fatalf("marshal issuances seed: %v", err)
	}
	if brokerSecret.Data == nil {
		brokerSecret.Data = map[string][]byte{}
	}
	brokerSecret.Data[IssuancesSecretKey] = payload
	if notFound {
		if _, err := k8s.CoreV1().Secrets(testBrokerNS).Create(context.Background(), brokerSecret, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create issuances secret: %v", err)
		}
	} else {
		if _, err := k8s.CoreV1().Secrets(testBrokerNS).Update(context.Background(), brokerSecret, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("update issuances secret: %v", err)
		}
	}
}

// worldByName returns the single-world testConfig() WorldConfig
// matching name. Sweeper tests use the default config's team-a world;
// tests that need multi-world fixtures construct the config inline.
func worldByName(t *testing.T, name string) *WorldConfig {
	t.Helper()
	cfg := testConfig()
	for i := range cfg.Worlds {
		if cfg.Worlds[i].Name == name {
			return &cfg.Worlds[i]
		}
	}
	t.Fatalf("no world named %q in testConfig", name)
	return nil
}

func TestSweepRetiresExpiredKeepsFresh(t *testing.T) {
	// Two issuances against team-a. "stale" expired 5 minutes before
	// the sweeper's clock; "fresh" expires 23 hours after. After one
	// sweep pass, the stale entry must be gone from both Secrets and
	// the fresh entry must be intact in both.
	k8s := fake.NewSimpleClientset()
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	seedIssuance(t, k8s, &Issuance{
		Label:      "usr_stale",
		Email:      "alice@example.com",
		World:      "team-a",
		Paths:      []string{"/team-a/*"},
		Operations: []string{"read"},
		IssuedAt:   now.Add(-24 * time.Hour),
		ExpiresAt:  now.Add(-5 * time.Minute),
	})
	seedIssuance(t, k8s, &Issuance{
		Label:      "usr_fresh",
		Email:      "alice@example.com",
		World:      "team-a",
		Paths:      []string{"/team-a/*"},
		Operations: []string{"read"},
		IssuedAt:   now.Add(-1 * time.Hour),
		ExpiresAt:  now.Add(23 * time.Hour),
	})

	issuer := newIssuer(t, testConfig(), k8s)
	s := NewSweeper(issuer, time.Hour, nil)
	s.clock = func() time.Time { return now }

	if err := s.sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	worldSecret, _ := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	gotTOML := string(worldSecret.Data[TokensSecretKey])
	if strings.Contains(gotTOML, "usr_stale") {
		t.Errorf("stale label still in world Secret:\n%s", gotTOML)
	}
	if !strings.Contains(gotTOML, "usr_fresh") {
		t.Errorf("fresh label missing from world Secret:\n%s", gotTOML)
	}

	live, err := issuer.List(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 || live[0].Label != "usr_fresh" {
		t.Errorf("issuances after sweep = %+v, want only usr_fresh", live)
	}
}

func TestSweepPrunesDrift(t *testing.T) {
	// Two issuances against team-a, both fresh (not expired). One has
	// no corresponding entry in the world's tokens Secret (operator
	// hand-edit or prior partial-Revoke). The sweeper must prune the
	// orphan from issuances; the other must stay intact in both
	// Secrets.
	k8s := fake.NewSimpleClientset()
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	seedIssuance(t, k8s, &Issuance{
		Label:      "usr_present",
		Email:      "alice@example.com",
		World:      "team-a",
		Paths:      []string{"/team-a/*"},
		Operations: []string{"read"},
		IssuedAt:   now,
		ExpiresAt:  now.Add(24 * time.Hour),
	})
	// usr_orphan: only the issuance record exists; the world's tokens
	// Secret does not contain its label. seedIssuanceRecord (broker
	// side only) plus seedIssuance for usr_present above already gave
	// us the world Secret with usr_present.
	seedIssuanceRecord(t, k8s, &Issuance{
		Label:      "usr_orphan",
		Email:      "alice@example.com",
		World:      "team-a",
		Paths:      []string{"/team-a/*"},
		Operations: []string{"read"},
		IssuedAt:   now,
		ExpiresAt:  now.Add(24 * time.Hour),
	})

	issuer := newIssuer(t, testConfig(), k8s)
	s := NewSweeper(issuer, time.Hour, nil)
	s.clock = func() time.Time { return now }

	if err := s.sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	live, err := issuer.List(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 || live[0].Label != "usr_present" {
		t.Errorf("issuances after drift sweep = %+v, want only usr_present", live)
	}
	worldSecret, _ := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if !strings.Contains(string(worldSecret.Data[TokensSecretKey]), "usr_present") {
		t.Errorf("present label missing from world Secret after drift sweep:\n%s", worldSecret.Data[TokensSecretKey])
	}
}

func TestSweepSkipsUnconfiguredWorld(t *testing.T) {
	// Issuance points at a world the broker has no config for —
	// admin removed the world between mint and sweep. Same
	// conservative posture as Revoke: surface and skip; never silently
	// drop. The issuance record must remain so an operator can
	// investigate.
	k8s := fake.NewSimpleClientset()
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	seedIssuanceRecord(t, k8s, &Issuance{
		Label:      "usr_orphan",
		Email:      "alice@example.com",
		World:      "team-deleted",
		Paths:      []string{"/x/*"},
		Operations: []string{"read"},
		IssuedAt:   now,
		ExpiresAt:  now.Add(24 * time.Hour),
	})

	issuer := newIssuer(t, testConfig(), k8s)
	s := NewSweeper(issuer, time.Hour, nil)
	s.clock = func() time.Time { return now }

	if err := s.sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	live, err := issuer.List(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 || live[0].Label != "usr_orphan" {
		t.Errorf("issuance dropped for unconfigured world: %+v", live)
	}
}

func TestSweepEmptyIssuancesIsNoop(t *testing.T) {
	// No issuances Secret exists yet — sweeper must return cleanly
	// without panicking on the nil-data path inside readIssuances or
	// touching any world Secret.
	k8s := fake.NewSimpleClientset()
	issuer := newIssuer(t, testConfig(), k8s)
	s := NewSweeper(issuer, time.Hour, nil)
	if err := s.sweep(context.Background()); err != nil {
		t.Fatalf("sweep on empty state: %v", err)
	}
	// No Secrets should have been created.
	if list, _ := k8s.CoreV1().Secrets("team-a").List(context.Background(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Errorf("empty sweep created team-a Secrets: %d", len(list.Items))
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
	cfg := testConfig()
	// Two issuers backed by the same fake clientset so leader
	// election state is shared. Each replica gets its own Sweeper
	// with its own counter.
	makeReplica := func(identity string) (*Sweeper, *atomic.Int32) {
		var count atomic.Int32
		s := NewSweeper(newIssuer(t, cfg, k8s), 30*time.Millisecond, nil)
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
		doneLeader                   chan struct{}
	)
	if leaderIsA {
		leaderCount, followerCount = countA, countB
		cancelLeader, cancelFollower = cancelA, cancelB
		doneLeader = doneA
	} else {
		leaderCount, followerCount = countB, countA
		cancelLeader, cancelFollower = cancelB, cancelA
		doneLeader = doneB
	}
	_ = cancelFollower

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
