package writepolicy

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// PolicySeed is a seed document (the default policy, the style guide) and
// the catalog metadata it is published with.
type PolicySeed struct {
	Body     []byte
	Metadata map[string]string
}

// ValidateSeed rejects a seed that would not survive a publish: wrong
// document shape, invalid write, or an unenforceable policy.
func ValidateSeed(seed PolicySeed) error {
	if err := storefmt.ValidateDocumentContent(publishpolicy.DocumentPath, seed.Body); err != nil {
		return fmt.Errorf("invalid policy document: %w", err)
	}
	if err := storefmt.ValidateWrite(seed.Body, seed.Metadata); err != nil {
		return fmt.Errorf("invalid policy write: %w", err)
	}
	if err := publishpolicy.Parse(string(seed.Body)).Validate(); err != nil {
		return fmt.Errorf("invalid policy: %w", err)
	}
	return nil
}

// Seed publishes seed as the world's policy when it has none and reports
// whether it created one. Create-only, so a restart never reverts a policy
// that replaced an earlier seed. Call it on the store before Enforce wraps it.
func Seed(ctx context.Context, store backend.Store, seed PolicySeed) (bool, error) {
	outcome, err := seedMissing(ctx, store, seed)
	return outcome.created, err
}

// Ensure is Seed followed by Validate in one read of the policy: a world that
// already holds one is judged by the read that found it.
func Ensure(ctx context.Context, store backend.Store, seed PolicySeed) (bool, error) {
	outcome, err := seedMissing(ctx, store, seed)
	switch {
	case err != nil || outcome.created:
		return outcome.created, err
	case outcome.lostCreate:
		// Another replica's policy stands; judge that one.
		return false, inspect(ctx, store)
	}
	return false, outcome.existing
}

// seedOutcome is what seeding found. existing is the verdict on a policy that
// was already there; an archived or broken one counts as present.
type seedOutcome struct {
	created    bool
	lostCreate bool
	existing   error
}

// seedMissing creates the policy only when the world has none.
func seedMissing(ctx context.Context, store backend.Store, seed PolicySeed) (seedOutcome, error) {
	verdict := inspect(ctx, store)
	switch {
	case verdict == nil:
		return seedOutcome{}, nil
	case errors.Is(verdict, errPolicyMissing):
	case errors.Is(verdict, ErrInvalidPolicy):
		return seedOutcome{existing: verdict}, nil
	default:
		return seedOutcome{}, fmt.Errorf("seed policy: %w", verdict)
	}
	if err := publishpolicy.Parse(string(seed.Body)).Validate(); err != nil {
		return seedOutcome{}, fmt.Errorf("invalid policy: %w", err)
	}
	created, err := EnsureDocument(ctx, store, publishpolicy.DocumentPath, seed)
	if err != nil {
		return seedOutcome{}, err
	}
	return seedOutcome{created: created, lostCreate: !created}, nil
}

// EnsureDocument creates seed at path and reports whether it did. The
// caller established absence; a conflict is a replica winning the create
// race, not an error.
func EnsureDocument(ctx context.Context, store backend.Store, path string, seed PolicySeed) (bool, error) {
	if err := storefmt.ValidateDocumentContent(path, seed.Body); err != nil {
		return false, fmt.Errorf("seed %s: invalid document: %w", path, err)
	}
	if err := storefmt.ValidateWrite(seed.Body, seed.Metadata); err != nil {
		return false, fmt.Errorf("seed %s: invalid write: %w", path, err)
	}
	request := backend.WriteRequest{Path: path, Content: seed.Body, Metadata: seed.Metadata}
	if _, err := store.Publish(ctx, request); err != nil {
		if errors.Is(err, storefmt.ErrConflict) {
			return false, nil
		}
		return false, fmt.Errorf("seed %s: create: %w", path, err)
	}
	return true, nil
}

// Exists reports whether the world holds a live document at path.
func Exists(ctx context.Context, store backend.Store, path string) (found bool, err error) {
	view, err := store.OpenReadView(ctx)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() {
		if closeErr := view.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("read %s: close view: %w", path, closeErr)
		}
	}()
	switch _, err := view.Get(ctx, path, 0); {
	case err == nil:
		return true, nil
	case errors.Is(err, backend.ErrNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("read %s: %w", path, err)
	}
}
