package storage

import (
	"bytes"
	"context"
	"fmt"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// maxConflictRetries bounds the read-modify-write loop on Secret
// resourceVersion conflicts; five converges under realistic contention
// while a stuck conflict still fails promptly.
const maxConflictRetries = 5

// NewK8sSecretStore returns the Kubernetes-backed SecretStore.
func NewK8sSecretStore(client kubernetes.Interface) core.SecretStore {
	return &k8sSecretStore{client: client}
}

type k8sSecretStore struct {
	client kubernetes.Interface
}

// Mutate performs an optimistic-concurrency read-modify-write on the Secret
// data[ref.Key], retrying resourceVersion conflicts up to maxConflictRetries.
func (s *k8sSecretStore) Mutate(ctx context.Context, ref core.SecretRef, mutate func([]byte) ([]byte, error)) error {
	for range maxConflictRetries {
		secret, getErr := s.client.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if getErr != nil && !apierrors.IsNotFound(getErr) {
			return fmt.Errorf("get secret %s/%s: %w", ref.Namespace, ref.Name, getErr)
		}
		var existing []byte
		if getErr == nil {
			existing = secret.Data[ref.Key]
		}
		next, err := mutate(existing)
		if err != nil {
			return err
		}
		if apierrors.IsNotFound(getErr) {
			// Absent stays absent: don't materialize an empty Secret as a
			// side effect of a no-op mutation (observable in helm-rendered
			// Secrets and audit logs).
			if len(next) == 0 {
				return nil
			}
			fresh := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ref.Name,
					Namespace: ref.Namespace,
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{ref.Key: next},
			}
			_, createErr := s.client.CoreV1().Secrets(ref.Namespace).Create(ctx, fresh, metav1.CreateOptions{})
			if createErr == nil {
				return nil
			}
			if apierrors.IsAlreadyExists(createErr) {
				continue
			}
			return fmt.Errorf("create secret %s/%s: %w", ref.Namespace, ref.Name, createErr)
		}
		// No-op mutation: skip the Update. Writing anyway bumps the
		// resourceVersion, triggering kubelet re-projection on every world
		// tokens.toml mount — the exact churn the broker works to avoid.
		if bytes.Equal(existing, next) {
			return nil
		}
		if secret.Data == nil {
			secret.Data = make(map[string][]byte, 1)
		}
		secret.Data[ref.Key] = next
		_, updateErr := s.client.CoreV1().Secrets(ref.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
		if updateErr == nil {
			return nil
		}
		if apierrors.IsConflict(updateErr) {
			continue
		}
		return fmt.Errorf("update secret %s/%s: %w", ref.Namespace, ref.Name, updateErr)
	}
	return fmt.Errorf("conflict on secret %s/%s after %d retries", ref.Namespace, ref.Name, maxConflictRetries)
}

// Delete removes ref.Key from the Secret, deleting the Secret itself
// when no keys remain: per-tenant Secrets would otherwise accumulate
// as empty husks across deprovisions.
func (s *k8sSecretStore) Delete(ctx context.Context, ref core.SecretRef) error {
	for range maxConflictRetries {
		secret, getErr := s.client.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			return nil
		}
		if getErr != nil {
			return fmt.Errorf("get secret %s/%s: %w", ref.Namespace, ref.Name, getErr)
		}
		if _, ok := secret.Data[ref.Key]; !ok {
			return nil
		}
		if len(secret.Data) == 1 {
			deleteErr := s.client.CoreV1().Secrets(ref.Namespace).Delete(ctx, ref.Name, metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{ResourceVersion: &secret.ResourceVersion},
			})
			if deleteErr == nil || apierrors.IsNotFound(deleteErr) {
				return nil
			}
			if apierrors.IsConflict(deleteErr) {
				continue
			}
			return fmt.Errorf("delete secret %s/%s: %w", ref.Namespace, ref.Name, deleteErr)
		}
		delete(secret.Data, ref.Key)
		_, updateErr := s.client.CoreV1().Secrets(ref.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
		if updateErr == nil {
			return nil
		}
		if apierrors.IsConflict(updateErr) {
			continue
		}
		return fmt.Errorf("update secret %s/%s: %w", ref.Namespace, ref.Name, updateErr)
	}
	return fmt.Errorf("conflict deleting secret %s/%s after %d retries", ref.Namespace, ref.Name, maxConflictRetries)
}
