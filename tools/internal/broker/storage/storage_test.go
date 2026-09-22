package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/latebit-io/demarkus/tools/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/tools/internal/broker/core"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestFileStoreCreateAndUpdate(t *testing.T) {
	dir := t.TempDir()
	ref := core.SecretRef{Path: filepath.Join(dir, "tokens.toml")}
	s := NewFileSecretStore()

	if err := s.Mutate(context.Background(), ref, func(existing []byte) ([]byte, error) {
		if len(existing) != 0 {
			t.Errorf("existing = %q, want empty on absent file", existing)
		}
		return []byte("v1"), nil
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := brokertest.MustRead(t, ref.Path); string(got) != "v1" {
		t.Fatalf("file = %q, want v1", got)
	}

	if err := s.Mutate(context.Background(), ref, func(existing []byte) ([]byte, error) {
		return append(existing, []byte("-v2")...), nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := brokertest.MustRead(t, ref.Path); string(got) != "v1-v2" {
		t.Errorf("file = %q, want v1-v2", got)
	}
}

func TestFileStoreAbsentStaysAbsent(t *testing.T) {
	dir := t.TempDir()
	ref := core.SecretRef{Path: filepath.Join(dir, "refresh_tokens.json")}
	s := NewFileSecretStore()

	if err := s.Mutate(context.Background(), ref, func([]byte) ([]byte, error) { return nil, nil }); err != nil {
		t.Fatalf("no-op mutate: %v", err)
	}
	if _, err := os.Stat(ref.Path); !os.IsNotExist(err) {
		t.Errorf("file materialized on empty-result no-op: %v", err)
	}
}

func TestFileStoreNoRewriteOnEqual(t *testing.T) {
	dir := t.TempDir()
	ref := core.SecretRef{Path: filepath.Join(dir, "tokens.toml")}
	if err := os.WriteFile(ref.Path, []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewFileSecretStore()
	if err := s.Mutate(context.Background(), ref, func(existing []byte) ([]byte, error) {
		return existing, nil
	}); err != nil {
		t.Fatalf("equal mutate: %v", err)
	}
	after, err := os.Stat(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Unchanged value must not be rewritten: the world server's tokens-file
	// watcher would reload on every no-op otherwise. SameFile catches a
	// rewrite that lands within the filesystem's mtime resolution.
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("file rewritten on unchanged value")
	}
	if !os.SameFile(before, after) {
		t.Error("file replaced (new inode) on unchanged value")
	}
}

func TestFileStorePreservesMode(t *testing.T) {
	dir := t.TempDir()
	ref := core.SecretRef{Path: filepath.Join(dir, "tokens.toml")}
	if err := os.WriteFile(ref.Path, []byte("v0"), 0o640); err != nil {
		t.Fatal(err)
	}
	s := NewFileSecretStore()
	if err := s.Mutate(context.Background(), ref, func([]byte) ([]byte, error) {
		return []byte("v1"), nil
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 640 (install.sh group-read preserved)", info.Mode().Perm())
	}
}

func TestFileStoreMutateErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	ref := core.SecretRef{Path: filepath.Join(dir, "f")}
	s := NewFileSecretStore()
	boom := errors.New("boom")
	if err := s.Mutate(context.Background(), ref, func([]byte) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	if _, statErr := os.Stat(ref.Path); !os.IsNotExist(statErr) {
		t.Error("file written despite mutate error")
	}
}

func TestFileStoreMissingPath(t *testing.T) {
	s := NewFileSecretStore()
	err := s.Mutate(context.Background(), core.SecretRef{Namespace: "ns", Name: "n", Key: "k"}, func([]byte) ([]byte, error) {
		return []byte("x"), nil
	})
	if err == nil {
		t.Fatal("expected error for ref without path")
	}
}

func TestFileStoreConcurrentMutations(t *testing.T) {
	dir := t.TempDir()
	ref := core.SecretRef{Path: filepath.Join(dir, "counter")}
	s := NewFileSecretStore()

	const n = 20
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			errs <- s.Mutate(context.Background(), ref, func(existing []byte) ([]byte, error) {
				return append(existing, 'x'), nil
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Mutate: %v", err)
		}
	}
	got, err := os.ReadFile(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Per-path locking makes read-modify-write atomic: no lost updates.
	if len(got) != n {
		t.Errorf("len = %d, want %d (lost updates under concurrency)", len(got), n)
	}
}

// TestMutateSecretConflictRetries: two Update conflicts in a row, then
// success; the loop re-reads and converges without surfacing them.
func TestMutateSecretConflictRetries(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-tokens", Namespace: "team-a"},
		Data:       map[string][]byte{core.TokensSecretKey: []byte("v0")},
	})
	var conflicts int32
	k8s.PrependReactor("update", "secrets", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		if atomic.LoadInt32(&conflicts) < 2 {
			atomic.AddInt32(&conflicts, 1)
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "team-a-tokens", errors.New("simulated"))
		}
		return false, nil, nil
	})

	err := NewK8sSecretStore(k8s).Mutate(context.Background(), core.SecretRef{Namespace: "team-a", Name: "team-a-tokens", Key: core.TokensSecretKey},
		func(existing []byte) ([]byte, error) {
			return append(append([]byte{}, existing...), []byte("-mutated")...), nil
		})
	if err != nil {
		t.Fatalf("mutateSecret: %v", err)
	}
	if got := atomic.LoadInt32(&conflicts); got != 2 {
		t.Errorf("simulated conflicts = %d, want 2 (retry-then-succeed)", got)
	}

	// The final write must reflect the mutate applied to the freshly
	// re-read value — proving the loop converged rather than clobbering.
	secret, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if got := string(secret.Data[core.TokensSecretKey]); got != "v0-mutated" {
		t.Errorf("secret value = %q, want %q", got, "v0-mutated")
	}
}

// TestMutateSecretConflictExhaustsRetries pins the terminal path: when
// every Update keeps conflicting, mutateSecret gives up after
// maxConflictRetries and surfaces the conflict rather than spinning.
func TestMutateSecretConflictExhaustsRetries(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-tokens", Namespace: "team-a"},
		Data:       map[string][]byte{core.TokensSecretKey: []byte("v0")},
	})
	var calls int32
	k8s.PrependReactor("update", "secrets", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		atomic.AddInt32(&calls, 1)
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "team-a-tokens", errors.New("simulated"))
	})

	err := NewK8sSecretStore(k8s).Mutate(context.Background(), core.SecretRef{Namespace: "team-a", Name: "team-a-tokens", Key: core.TokensSecretKey},
		func(existing []byte) ([]byte, error) { return append(existing, 'x'), nil })
	if err == nil {
		t.Fatal("mutateSecret returned nil, want conflict error after retry budget")
	}
	if got := atomic.LoadInt32(&calls); got != int32(maxConflictRetries) {
		t.Errorf("Update called %d times, want %d (full retry budget)", got, maxConflictRetries)
	}
}

// TestMutateSecretCreatesWhenAbsent covers the create branch: when the
// Secret does not exist and the mutate yields a non-empty payload,
// mutateSecret materializes a fresh Secret with the key set.
func TestMutateSecretCreatesWhenAbsent(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	err := NewK8sSecretStore(k8s).Mutate(context.Background(), core.SecretRef{Namespace: "team-a", Name: "team-a-tokens", Key: core.TokensSecretKey},
		func(existing []byte) ([]byte, error) {
			if len(existing) != 0 {
				t.Errorf("existing = %q, want empty on absent Secret", existing)
			}
			return []byte("created"), nil
		})
	if err != nil {
		t.Fatalf("mutateSecret: %v", err)
	}
	secret, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if got := string(secret.Data[core.TokensSecretKey]); got != "created" {
		t.Errorf("secret value = %q, want %q", got, "created")
	}
}

// TestMutateSecretAbsentStaysAbsentOnEmptyResult pins the no write
// contract the refresh store's Revoke and Sweep rely on.
func TestMutateSecretAbsentStaysAbsentOnEmptyResult(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	err := NewK8sSecretStore(k8s).Mutate(context.Background(), core.SecretRef{Namespace: "team-a", Name: "team-a-tokens", Key: core.TokensSecretKey},
		func([]byte) ([]byte, error) { return nil, nil })
	if err != nil {
		t.Fatalf("mutateSecret: %v", err)
	}
	if _, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Secret unexpectedly created on empty-result no-op: %v", err)
	}
}
