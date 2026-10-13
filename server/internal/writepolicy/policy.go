// Package writepolicy enforces a world's publish policy above any backend. The
// verdict runs as a write precondition, inside the backend's own commit.
package writepolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/protocol/layout"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

var (
	// ErrPolicyBlocked reports a blocked noncompliant mutation.
	ErrPolicyBlocked = errors.New("publish policy blocked mutation")
	// ErrPolicyApprovalRequired reports a noninteractive ask decision.
	ErrPolicyApprovalRequired = errors.New("publish policy requires approval")
	// ErrInvalidPolicy reports a missing or malformed required policy.
	ErrInvalidPolicy = errors.New("invalid publish policy")
	// errPolicyMissing narrows ErrInvalidPolicy to a world with no document.
	errPolicyMissing = errors.New("policy document is missing")
)

// PolicyError carries the violations so the handler can list them.
type PolicyError struct {
	Strictness publishpolicy.Strictness
	Result     publishpolicy.Result
	Policy     publishpolicy.Policy
}

func (err *PolicyError) Error() string {
	return fmt.Sprintf("publish policy %s: %d violation(s)", err.Strictness, len(err.Result.Violations))
}

func (err *PolicyError) Unwrap() []error {
	if err.Strictness == publishpolicy.Ask {
		return []error{ErrPolicyApprovalRequired, backend.ErrRejected}
	}
	return []error{ErrPolicyBlocked, backend.ErrRejected}
}

// RejectionMessage lists the violations so the publisher can correct them.
func (err *PolicyError) RejectionMessage() string {
	var b strings.Builder
	fmt.Fprintf(&b, "publish policy (%s) refused this write:\n", err.Strictness)
	for _, v := range err.Result.Violations {
		switch {
		case v.Name == "":
			fmt.Fprintf(&b, "- %s\n", v.Code)
		case v.Axis != "":
			fmt.Fprintf(&b, "- %s: %s (allowed %s values: %s)\n", v.Code, v.Name, v.Axis, err.Policy.AllowedList(v.Axis))
		default:
			fmt.Fprintf(&b, "- %s: %s\n", v.Code, v.Name)
		}
	}
	return b.String()
}

// Current loads and validates the policy document from reader. With require
// false a world without one has the empty policy.
func Current(ctx context.Context, reader backend.Reader, require bool) (publishpolicy.Policy, error) {
	document, err := reader.Get(ctx, publishpolicy.DocumentPath, 0)
	switch {
	case errors.Is(err, backend.ErrNotFound):
		if require {
			return publishpolicy.Policy{}, fmt.Errorf("%w: %w: %s", ErrInvalidPolicy, errPolicyMissing, publishpolicy.DocumentPath)
		}
		return publishpolicy.Policy{}, nil
	case err != nil:
		return publishpolicy.Policy{}, fmt.Errorf("load current policy: %w", err)
	case document.Archived:
		return publishpolicy.Policy{}, fmt.Errorf("%w: %s is archived", ErrInvalidPolicy, publishpolicy.DocumentPath)
	}
	policy := publishpolicy.Parse(string(document.Content))
	if err := policy.Validate(); err != nil {
		return publishpolicy.Policy{}, errors.Join(
			storefmt.ErrIntegrity,
			fmt.Errorf("%w: current policy: %v", ErrInvalidPolicy, err),
		)
	}
	return policy, nil
}

// evaluate judges one prepared write: the body for secrets and personal
// data first, so a refused body is reported as such, then the metadata under
// the floor merged with this world's policy. A candidate policy must be
// enforceable and satisfiable under the floor.
func (e *enforcer) evaluate(ctx context.Context, state backend.Reader, write storefmt.PreparedWrite) error {
	if err := e.content.judge(write); err != nil {
		return err
	}
	var floor *publishpolicy.Policy
	if e.options.Floor != nil {
		read, err := e.options.Floor(ctx)
		if err != nil {
			return fmt.Errorf("%w: read floor policy: %w", ErrInvalidPolicy, err)
		}
		floor = &read
	}
	if write.Path == publishpolicy.DocumentPath {
		candidate := publishpolicy.Parse(string(write.Content))
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("%w: %w: candidate policy: %v", backend.ErrRejected, ErrInvalidPolicy, err)
		}
		if err := e.floorSatisfiable(ctx, candidate); err != nil {
			return err
		}
		// A world list disjoint from the floor's would refuse every value on
		// that axis; refuse the candidate instead.
		if floor != nil {
			if err := publishpolicy.Merge(*floor, candidate).Validate(); err != nil {
				return fmt.Errorf("%w: %w: candidate policy merged under the floor: %v", backend.ErrRejected, ErrInvalidPolicy, err)
			}
		}
	}
	// A template with a bad layout line would silently route writes to the
	// default directories; refuse it the way a bad policy is refused.
	if write.Path == layout.DocumentPath {
		if _, err := layout.Parse(string(write.Content)); err != nil {
			return fmt.Errorf("%w: %w: candidate template: %v", backend.ErrRejected, ErrInvalidPolicy, err)
		}
	}
	policy, err := Current(ctx, state, e.options.Require)
	if err != nil {
		return err
	}
	if floor != nil {
		policy = publishpolicy.Merge(*floor, policy)
	}
	result := publishpolicy.EvaluateStrings(policy, write.Path, write.Metadata)
	strictness := policy.EffectiveStrictness()
	if result.Compliant() || strictness == publishpolicy.Warn {
		return nil
	}
	return &PolicyError{Strictness: strictness, Result: result, Policy: policy}
}

// floorSatisfiable refuses a candidate floor policy that, merged under any
// world's current policy, could not be met within the metadata budget.
func (e *enforcer) floorSatisfiable(ctx context.Context, candidate publishpolicy.Policy) error {
	if e.options.Worlds == nil {
		return nil
	}
	worlds, err := e.options.Worlds(ctx)
	if err != nil {
		return fmt.Errorf("%w: read world policies: %w", ErrInvalidPolicy, err)
	}
	for _, world := range worlds {
		if err := publishpolicy.Merge(candidate, world).Validate(); err != nil {
			return fmt.Errorf("%w: %w: candidate floor policy merged with a world's: %v", backend.ErrRejected, ErrInvalidPolicy, err)
		}
	}
	return nil
}
