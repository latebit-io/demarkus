// Package writepolicy enforces a world's publish policy above any backend. The
// verdict runs as a write precondition, inside the backend's own commit.
package writepolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
)

// PolicyError carries the violations so the handler can list them.
type PolicyError struct {
	Strictness publishpolicy.Strictness
	Result     publishpolicy.Result
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
		if v.Name == "" {
			fmt.Fprintf(&b, "- %s\n", v.Code)
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", v.Code, v.Name)
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
			return publishpolicy.Policy{}, fmt.Errorf("%w: %s is missing", ErrInvalidPolicy, publishpolicy.DocumentPath)
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

// evaluate judges one prepared write against the policy it commits under. A
// write of the policy document must itself be an enforceable policy.
func evaluate(ctx context.Context, state backend.Reader, write storefmt.PreparedWrite, require bool) error {
	if write.Path == publishpolicy.DocumentPath {
		if err := publishpolicy.Parse(string(write.Content)).Validate(); err != nil {
			return fmt.Errorf("%w: %w: candidate policy: %v", backend.ErrRejected, ErrInvalidPolicy, err)
		}
	}
	policy, err := Current(ctx, state, require)
	if err != nil {
		return err
	}
	values := make(map[string]any, len(write.Metadata))
	for key, value := range write.Metadata {
		values[key] = value
	}
	result := publishpolicy.Evaluate(policy, write.Path, values)
	strictness := policy.EffectiveStrictness()
	if result.Compliant() || strictness == publishpolicy.Warn {
		return nil
	}
	return &PolicyError{Strictness: strictness, Result: result}
}
