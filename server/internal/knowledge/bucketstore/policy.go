package bucketstore

import (
	"errors"
	"fmt"
	"maps"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
)

var (
	// ErrPolicyBlocked reports a blocked noncompliant mutation.
	ErrPolicyBlocked = errors.New("publish policy blocked mutation")
	// ErrPolicyApprovalRequired reports a noninteractive ask decision.
	ErrPolicyApprovalRequired = errors.New("publish policy requires approval")
	// ErrInvalidPolicy reports a missing or malformed required policy.
	ErrInvalidPolicy = errors.New("invalid publish policy")
)

// MutationResult carries knowledge-only policy output beside a store mutation.
type MutationResult struct {
	Document   *protocolstore.Document
	Changed    bool
	Strictness publishpolicy.Strictness
	Policy     publishpolicy.Result
}

// PolicyError preserves deterministic violations for the knowledge handler.
type PolicyError struct {
	Strictness publishpolicy.Strictness
	Result     publishpolicy.Result
}

func (err *PolicyError) Error() string {
	return fmt.Sprintf("publish policy %s: %d violation(s)", err.Strictness, len(err.Result.Violations))
}

func (err *PolicyError) Unwrap() error {
	if err.Strictness == publishpolicy.Ask {
		return ErrPolicyApprovalRequired
	}
	return ErrPolicyBlocked
}

func (view *readView) currentPolicy(require bool) (publishpolicy.Policy, error) {
	entry, exists := view.snapshot.Paths[publishpolicy.DocumentPath]
	if !exists {
		if require {
			return publishpolicy.Policy{}, fmt.Errorf("%w: %s is missing", ErrInvalidPolicy, publishpolicy.DocumentPath)
		}
		return publishpolicy.Policy{}, nil
	}
	if entry.Archived {
		return publishpolicy.Policy{}, fmt.Errorf("%w: %s is archived", ErrInvalidPolicy, publishpolicy.DocumentPath)
	}
	document, err := view.Get(publishpolicy.DocumentPath, 0)
	if err != nil {
		return publishpolicy.Policy{}, fmt.Errorf("load current policy: %w", err)
	}
	policy := publishpolicy.Parse(string(document.Content))
	if err := policy.Validate(); err != nil {
		return publishpolicy.Policy{}, errors.Join(
			protocolstore.ErrIntegrity,
			fmt.Errorf("%w: current policy: %v", ErrInvalidPolicy, err),
		)
	}
	return policy, nil
}

func evaluateMutationPolicy(
	view *readView,
	require bool,
	path string,
	metadata map[string]string,
	body []byte,
) (publishpolicy.Strictness, publishpolicy.Result, error) {
	if path == publishpolicy.DocumentPath {
		candidate := publishpolicy.Parse(string(body))
		if err := candidate.Validate(); err != nil {
			return "", publishpolicy.Result{}, fmt.Errorf("%w: candidate policy: %v", ErrInvalidPolicy, err)
		}
	}
	policy, err := view.currentPolicy(require)
	if err != nil {
		return "", publishpolicy.Result{}, err
	}
	values := make(map[string]any, len(metadata))
	for key, value := range maps.Clone(metadata) {
		values[key] = value
	}
	result := publishpolicy.Evaluate(policy, path, values)
	strictness := policy.EffectiveStrictness()
	if result.Compliant() || strictness == publishpolicy.Warn {
		return strictness, result, nil
	}
	return strictness, result, &PolicyError{Strictness: strictness, Result: result}
}
