package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// SecretRef names one credential document in both backends: the k8s
// (Namespace, Name, Key) Secret tuple and the file backend's Path. Refs are
// built by the Config helpers so every call site resolves locations one way.
type SecretRef struct {
	Namespace string
	Name      string
	Key       string
	Path      string
}

func (r SecretRef) String() string {
	if r.Path != "" {
		return r.Path
	}
	return r.Namespace + "/" + r.Name + "[" + r.Key + "]"
}

// SecretStore is the broker's credential-persistence backend: an atomic
// read-modify-write on one credential document. Contract shared by both
// implementations: a mutation that leaves an absent document empty writes
// nothing (absent stays absent), and an unchanged value is not rewritten
// (no spurious version/mtime churn for watchers).
type SecretStore interface {
	Mutate(ctx context.Context, ref SecretRef, mutate func(current []byte) ([]byte, error)) error
}

// NewK8sSecretStore returns the Kubernetes-backed SecretStore.
func NewK8sSecretStore(client kubernetes.Interface) SecretStore {
	return &k8sSecretStore{client: client}
}

type k8sSecretStore struct {
	client kubernetes.Interface
}

// Mutate performs an optimistic-concurrency read-modify-write on the Secret
// data[ref.Key], retrying resourceVersion conflicts up to maxConflictRetries.
func (s *k8sSecretStore) Mutate(ctx context.Context, ref SecretRef, mutate func([]byte) ([]byte, error)) error {
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

// NewFileSecretStore returns the file-backed SecretStore for single-host
// mode. Writes are atomic (temp file + rename, preserving an existing file's
// mode) so the world server's tokens-file watcher, which watches the parent
// directory, sees exactly one complete replacement per mutation. Concurrency
// is guarded per path within this process; a concurrent out-of-process
// writer (an operator running demarkus-token by hand) has the same
// last-write-wins exposure as hand-editing tokens.toml today.
func NewFileSecretStore() SecretStore {
	return &fileSecretStore{locks: make(map[string]*sync.Mutex)}
}

type fileSecretStore struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (s *fileSecretStore) lockFor(path string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[path]
	if !ok {
		l = &sync.Mutex{}
		s.locks[path] = l
	}
	return l
}

func (s *fileSecretStore) Mutate(_ context.Context, ref SecretRef, mutate func([]byte) ([]byte, error)) error {
	if ref.Path == "" {
		return fmt.Errorf("file secret store: ref %s has no path (storage.dir or world tokensFile missing)", ref)
	}
	l := s.lockFor(ref.Path)
	l.Lock()
	defer l.Unlock()

	existing, readErr := os.ReadFile(ref.Path)
	absent := false
	if readErr != nil {
		if !os.IsNotExist(readErr) {
			return fmt.Errorf("read %s: %w", ref.Path, readErr)
		}
		absent = true
		existing = nil
	}
	next, err := mutate(existing)
	if err != nil {
		return err
	}
	if absent && len(next) == 0 {
		return nil
	}
	if !absent && bytes.Equal(existing, next) {
		return nil
	}

	// Preserve a pre-existing file's mode and ownership: rename would
	// otherwise re-own it to the broker user, revoking the world server's
	// group-read on tokens.toml. New files are owner-only.
	mode := os.FileMode(0o600)
	var owner *fileOwner
	if info, statErr := os.Stat(ref.Path); statErr == nil {
		mode = info.Mode().Perm()
		owner = ownerOf(info)
	} else if !os.IsNotExist(statErr) {
		// A file that exists but cannot be statted must not be replaced
		// with downgraded metadata.
		return fmt.Errorf("stat %s: %w", ref.Path, statErr)
	}
	dir := filepath.Dir(ref.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(ref.Path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", ref.Path, err)
	}
	tmpName := tmp.Name()
	fail := func(step string, cause error) error {
		return errors.Join(
			fmt.Errorf("%s for %s: %w", step, ref.Path, cause),
			removeIfPresent(tmpName),
		)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fail("chmod temp", err)
	}
	if owner != nil {
		if err := chownPreserving(tmpName, owner); err != nil {
			_ = tmp.Close()
			return fail("preserve ownership", err)
		}
	}
	if _, err := tmp.Write(next); err != nil {
		_ = tmp.Close()
		return fail("write temp", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fail("sync temp", err)
	}
	if err := tmp.Close(); err != nil {
		return fail("close temp", err)
	}
	if err := os.Rename(tmpName, ref.Path); err != nil {
		return fail("replace", err)
	}
	// Sync the directory so the rename itself survives a crash: file
	// contents were fsynced above, but the new directory entry was not.
	return syncDir(dir)
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cleanup temp %s: %w", path, err)
	}
	return nil
}

// chownPreserving restores the original owner; when uid restoration needs
// privilege the gid alone keeps the shared-group read policy, so that
// degradation is accepted. Any other failure errors rather than re-owning.
func chownPreserving(path string, owner *fileOwner) error {
	if err := os.Chown(path, owner.uid, owner.gid); err == nil {
		return nil
	}
	if err := os.Chown(path, -1, owner.gid); err != nil {
		return fmt.Errorf("cannot preserve owner uid=%d gid=%d (run the broker as the file's owner or a member of its group): %w", owner.uid, owner.gid, err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir %s for sync: %w", dir, err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return fmt.Errorf("sync dir %s: %w", dir, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close dir %s: %w", dir, closeErr)
	}
	return nil
}

// Ref builders: one place resolves every credential document's location for
// both backends. Path fields are populated only in file-backend mode.

func (c *Config) refreshTokensRef() SecretRef {
	ref := SecretRef{
		Namespace: c.Server.BrokerNamespace,
		Name:      c.Server.RefreshTokensSecret,
		Key:       RefreshTokensSecretKey,
	}
	if c.fileBackend() {
		ref.Path = filepath.Join(c.Storage.Dir, RefreshTokensSecretKey)
	}
	return ref
}

func (c *Config) worldWriteTokenRef(worldName string) SecretRef {
	ref := SecretRef{
		Namespace: c.Server.BrokerNamespace,
		Name:      worldWriteTokenSecretName(worldName),
		Key:       worldWriteTokenSecretKey,
	}
	if c.fileBackend() {
		ref.Path = filepath.Join(c.Storage.Dir, worldWriteTokenSecretName(worldName)+".json")
	}
	return ref
}

func (c *Config) worldTokensRef(world *WorldConfig) SecretRef {
	return SecretRef{
		Namespace: world.Namespace,
		Name:      world.TokensSecret,
		Key:       TokensSecretKey,
		Path:      world.TokensFile,
	}
}
