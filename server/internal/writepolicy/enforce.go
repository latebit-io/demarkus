package writepolicy

import (
	"context"
	"fmt"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// Options configures enforcement.
type Options struct {
	// Require makes a missing policy document a refusal instead of the
	// empty policy.
	Require bool
	// Floor reads the floor world's policy, merged under this world's so
	// the world can only tighten it. Nil means no floor. An error refuses
	// the write: a floor that cannot be read is not skipped.
	Floor func(ctx context.Context) (publishpolicy.Policy, error)
	// Worlds lists the policies of every world under this one's floor; set
	// on the floor world so a candidate floor is refused when it would
	// leave a world unsatisfiable. Nil skips the check.
	Worlds func(ctx context.Context) ([]publishpolicy.Policy, error)
	// Content is the secrets and personal-data scan.
	Content ContentOptions
}

// Enforce wraps store so every PUBLISH and APPEND is judged by the world's
// policy document. Reads pass through untouched.
func Enforce(store backend.Store, options Options) backend.Store {
	return &enforcer{Store: store, options: options, content: newContentGate(options.Content)}
}

type enforcer struct {
	backend.Store
	options Options
	content contentGate
}

// chain runs the caller's precondition, then the enforcer's.
func chain[T any](inner, next func(context.Context, backend.Reader, T) error) func(context.Context, backend.Reader, T) error {
	if inner == nil {
		return next
	}
	return func(ctx context.Context, state backend.Reader, change T) error {
		if err := inner(ctx, state, change); err != nil {
			return err
		}
		return next(ctx, state, change)
	}
}

// guard adds the policy verdict after any precondition the caller set.
func (e *enforcer) guard(req *backend.WriteRequest) {
	req.Precondition = chain(req.Precondition, func(ctx context.Context, state backend.Reader, write storefmt.PreparedWrite) error {
		return e.evaluate(ctx, state, write)
	})
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
// refuse every write. The guard runs inside the commit, after the caller's own.
func (e *enforcer) SetArchived(ctx context.Context, req backend.ArchiveRequest) (backend.ArchiveResult, error) {
	req.Precondition = chain(req.Precondition, e.keepRequiredPolicy)
	return e.Store.SetArchived(ctx, req)
}

func (e *enforcer) keepRequiredPolicy(_ context.Context, _ backend.Reader, change storefmt.ArchiveChange) error {
	if change.Archived && e.options.Require && change.Path == publishpolicy.DocumentPath {
		return fmt.Errorf("%w: %w: required policy cannot be archived", backend.ErrRejected, ErrInvalidPolicy)
	}
	return nil
}

// Validate reports whether the world holds a usable policy document, which a
// server checks before it starts enforcing one.
func Validate(ctx context.Context, store backend.Store) error {
	return inspect(ctx, store)
}

// inspect reads the current policy through one view and returns its verdict.
func inspect(ctx context.Context, store backend.Store) error {
	_, err := Read(ctx, store, true)
	return err
}

// Read is Current through one view of store.
func Read(ctx context.Context, store backend.Store, require bool) (policy publishpolicy.Policy, err error) {
	view, err := store.OpenReadView(ctx)
	if err != nil {
		return publishpolicy.Policy{}, fmt.Errorf("read policy: %w", err)
	}
	defer func() {
		if closeErr := view.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("read policy: close view: %w", closeErr)
		}
	}()
	return Current(ctx, view, require)
}
