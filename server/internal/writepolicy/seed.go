package writepolicy

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// PolicySeed is a default policy document and the catalog metadata it is
// published with.
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
	if err := ValidateSeed(seed); err != nil {
		return seedOutcome{}, err
	}
	request := backend.WriteRequest{Path: publishpolicy.DocumentPath, Content: seed.Body, Metadata: seed.Metadata}
	if _, err := store.Publish(ctx, request); err != nil {
		if errors.Is(err, storefmt.ErrConflict) {
			return seedOutcome{lostCreate: true}, nil
		}
		return seedOutcome{}, fmt.Errorf("create policy: %w", err)
	}
	return seedOutcome{created: true}, nil
}
