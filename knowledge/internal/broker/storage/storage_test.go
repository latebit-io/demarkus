package storage

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

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
