package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/latebit-io/demarkus/server/blob"
)

func indexes(n int) []int {
	all := make([]int, n)
	for index := range all {
		all[index] = index
	}
	return all
}

// getValidated reads the object at a fixed key and returns it with its
// bytes; a defect in its encoding or content is an integrity failure.
func getValidated[T any](ctx context.Context, objects blob.Store, key string, validate func(*T) error) (T, error) {
	object, err := objects.Get(ctx, key)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("read %q: %w", key, err)
	}
	return decodeValidated(key, &object, validate)
}

// decodeValidated checks an object read at key and decodes it.
func decodeValidated[T any](key string, object *blob.Object, validate func(*T) error) (value T, err error) {
	if err := validateReadObject(key, object); err != nil {
		return value, err
	}
	return decodeChecked(key, object.Data, validate)
}

// runParallel applies fn to every job on up to workers goroutines. The first
// error cancels the rest and is returned; fn's errors are not retried.
func runParallel[T any](ctx context.Context, workers int, jobs []T, fn func(ctx context.Context, job T) error) error {
	if workers < 1 {
		return fmt.Errorf("%w: workers must be positive", blob.ErrPrecondition)
	}
	if len(jobs) == 0 {
		return nil
	}
	workCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	queue := make(chan T, len(jobs))
	for _, job := range jobs {
		queue <- job
	}
	close(queue)
	var wait sync.WaitGroup
	for range min(workers, len(jobs)) {
		wait.Go(func() {
			for job := range queue {
				if workCtx.Err() != nil {
					return
				}
				if err := fn(workCtx, job); err != nil {
					cancel(err)
					return
				}
			}
		})
	}
	wait.Wait()
	return context.Cause(workCtx)
}

// keyedRef is a stored reference and the key its kind and hash must produce.
type keyedRef struct {
	objectRef
	expectedKey string
}

func getImmutable[T any](ctx context.Context, objects objectGetter, keyed keyedRef, validate func(*T) error) (T, error) {
	object, err := getVerified(ctx, objects, keyed)
	if err != nil {
		var zero T
		return zero, err
	}
	return decodeChecked(keyed.Key, object.Data, validate)
}

// getVerified reads a referenced object and checks its bytes hash to it.
func getVerified(ctx context.Context, objects objectGetter, keyed keyedRef) (blob.Object, error) {
	object, err := getReferenced(ctx, objects, keyed)
	if err == nil {
		err = verifyBlobHash(keyed.objectRef, object.Data)
	}
	if err != nil {
		return blob.Object{}, err
	}
	return object, nil
}

// getReferenced is getVerified without the hash check, for a caller that
// verifies the bytes another way: a missing object is an integrity failure
// that also matches blob.ErrNotFound.
func getReferenced(ctx context.Context, objects objectGetter, keyed keyedRef) (blob.Object, error) {
	ref := keyed.objectRef
	if err := verifyRef(ref, keyed.expectedKey); err != nil {
		return blob.Object{}, fmt.Errorf("%w: %v", blob.ErrIntegrity, err)
	}
	object, err := objects.Get(ctx, ref.Key)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return blob.Object{}, fmt.Errorf("%w: referenced object %q is missing: %w", blob.ErrIntegrity, ref.Key, err)
		}
		return blob.Object{}, fmt.Errorf("read referenced object %q: %w", ref.Key, err)
	}
	if err := validateReadObject(ref.Key, &object); err != nil {
		return blob.Object{}, err
	}
	return object, nil
}

// decodeChecked decodes the canonical bytes read at key and validates them.
func decodeChecked[T any](key string, data []byte, validate func(*T) error) (T, error) {
	var result T
	if err := decodeImmutable(data, &result); err != nil {
		return result, fmt.Errorf("%w: decode object %q: %v", blob.ErrIntegrity, key, err)
	}
	if err := validate(&result); err != nil {
		return result, fmt.Errorf("%w: validate object %q: %v", blob.ErrIntegrity, key, err)
	}
	return result, nil
}

func validateReadObject(key string, object *blob.Object) error {
	attributes := object.Attributes
	if attributes.Key != key || attributes.Generation <= 0 || attributes.Size != int64(len(object.Data)) || attributes.Modified.IsZero() {
		return fmt.Errorf("%w: object %q has inconsistent attributes", blob.ErrIntegrity, key)
	}
	return nil
}
