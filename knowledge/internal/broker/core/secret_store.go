package core

import "context"

// SecretRef names one credential document: the (Namespace, Name, Key) Secret
// tuple. Refs are built by the Config helpers so every call site resolves
// locations one way.
type SecretRef struct {
	Namespace string
	Name      string
	Key       string
}

func (r SecretRef) String() string {
	return r.Namespace + "/" + r.Name + "[" + r.Key + "]"
}

// SecretStore is an atomic read-modify-write on one credential document.
// An absent document left empty stays absent, and an unchanged value is
// not rewritten.
type SecretStore interface {
	Mutate(ctx context.Context, ref SecretRef, mutate func(current []byte) ([]byte, error)) error
	// Delete removes the credential document (the key; the Secret too
	// when it holds no other keys). Absent is success: deprovisioning
	// converges on reruns.
	Delete(ctx context.Context, ref SecretRef) error
}

// ReadSecret returns ref's current value, nil when absent. An identity Mutate
// never writes: absent stays absent and an unchanged value is skipped.
func ReadSecret(ctx context.Context, store SecretStore, ref SecretRef) ([]byte, error) {
	var value []byte
	err := store.Mutate(ctx, ref, func(current []byte) ([]byte, error) {
		value = current
		return current, nil
	})
	return value, err
}

// EnsureCreated returns ref's value, storing gen's output when absent, and
// whether this call generated it. Create-only, so racing callers converge.
func EnsureCreated(ctx context.Context, store SecretStore, ref SecretRef, gen func() ([]byte, error)) (value []byte, generated bool, err error) {
	err = store.Mutate(ctx, ref, func(current []byte) ([]byte, error) {
		// Mutate retries on conflict; only the final attempt's outcome counts.
		if len(current) > 0 {
			value, generated = current, false
			return current, nil
		}
		fresh, genErr := gen()
		if genErr != nil {
			return nil, genErr
		}
		value, generated = fresh, true
		return fresh, nil
	})
	if err != nil {
		return nil, false, err
	}
	return value, generated, nil
}
