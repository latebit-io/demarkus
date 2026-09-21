package bucketstore

import (
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

// MutationResult carries knowledge-only policy output beside a store mutation.
type MutationResult struct {
	Document   *storefmt.Document
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
			storefmt.ErrIntegrity,
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
			return "", publishpolicy.Result{}, fmt.Errorf("%w: %w: candidate policy: %v", backend.ErrRejected, ErrInvalidPolicy, err)
		}
	}
	policy, err := view.currentPolicy(require)
	if err != nil {
		return "", publishpolicy.Result{}, err
	}
	values := make(map[string]any, len(metadata))
	for key, value := range metadata {
		values[key] = value
	}
	result := publishpolicy.Evaluate(policy, path, values)
	strictness := policy.EffectiveStrictness()
	if result.Compliant() || strictness == publishpolicy.Warn {
		return strictness, result, nil
	}
	return strictness, result, &PolicyError{Strictness: strictness, Result: result}
}
