package core

import "context"

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

// SecretStore is an atomic read-modify-write on one credential document.
// Both backends share the contract: an absent document left empty stays
// absent, and an unchanged value is not rewritten.
type SecretStore interface {
	Mutate(ctx context.Context, ref SecretRef, mutate func(current []byte) ([]byte, error)) error
	// Delete removes the credential document (the key; the Secret too
	// when it holds no other keys). Absent is success: deprovisioning
	// converges on reruns.
	Delete(ctx context.Context, ref SecretRef) error
}
