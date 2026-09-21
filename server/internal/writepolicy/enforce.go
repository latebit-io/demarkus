package writepolicy

import (
	"context"
	"fmt"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// Options configures enforcement. Require makes a missing policy document a
// refusal instead of the empty policy.
type Options struct {
	Require bool
}

// Enforce wraps store so every PUBLISH and APPEND is judged by the world's
// policy document. Reads pass through untouched.
func Enforce(store backend.Store, options Options) backend.Store {
	return &enforcer{Store: store, require: options.Require}
}

type enforcer struct {
	backend.Store
	require bool
}

// guard adds the policy verdict after any precondition the caller set.
func (e *enforcer) guard(req *backend.WriteRequest) {
	inner := req.Precondition
	req.Precondition = func(ctx context.Context, state backend.Reader, write storefmt.PreparedWrite) error {
		if inner != nil {
			if err := inner(ctx, state, write); err != nil {
				return err
			}
		}
		return evaluate(ctx, state, write, e.require)
	}
}

func (e *enforcer) Publish(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	e.guard(&req)
	return e.Store.Publish(ctx, req)
}

func (e *enforcer) Append(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	e.guard(&req)
	return e.Store.Append(ctx, req)
}

// SetArchived refuses to archive a required policy: the world would then
// refuse every write. The rule needs no state, so it runs before the store.
func (e *enforcer) SetArchived(ctx context.Context, reqPath string, archived bool) (backend.ArchiveResult, error) {
	if archived && e.require && storefmt.CanonicalPath(reqPath) == publishpolicy.DocumentPath {
		return backend.ArchiveResult{}, fmt.Errorf("%w: %w: required policy cannot be archived", backend.ErrRejected, ErrInvalidPolicy)
	}
	return e.Store.SetArchived(ctx, reqPath, archived)
}

// Validate reports whether the world holds a usable policy document, which a
// server checks before it starts enforcing one.
func Validate(ctx context.Context, store backend.Store) (err error) {
	view, err := store.OpenReadView(ctx)
	if err != nil {
		return fmt.Errorf("validate policy: %w", err)
	}
	defer func() {
		if closeErr := view.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("validate policy: close view: %w", closeErr)
		}
	}()
	_, err = Current(ctx, view, true)
	return err
}
