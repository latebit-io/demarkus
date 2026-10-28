// Package writepolicy enforces a world's publish policy above any backend. The
// verdict runs as a write precondition, inside the backend's own commit.
package writepolicy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/protocol/layout"
	"github.com/latebit-io/demarkus/protocol/provenance"
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
	for _, v := range violationLines(err.Result, err.Policy) {
		fmt.Fprintf(&b, "- %s\n", v)
	}
	return b.String()
}

// violationLines words each violation once, for a refusal or a warning.
func violationLines(result publishpolicy.Result, policy publishpolicy.Policy) []string {
	lines := make([]string, 0, len(result.Violations))
	for _, v := range result.Violations {
		switch {
		case v.Name == "":
			lines = append(lines, string(v.Code))
		case v.Code == publishpolicy.UnpinnedSource:
			lines = append(lines, fmt.Sprintf("%s: %s (pin %s)", v.Code, v.Name, provenance.PinForms))
		case v.Axis != "":
			lines = append(lines, fmt.Sprintf("%s: %s (allowed %s values: %s)", v.Code, v.Name, v.Axis, policy.AllowedList(v.Axis)))
		default:
			lines = append(lines, fmt.Sprintf("%s: %s", v.Code, v.Name))
		}
	}
	return lines
}

// policyWarning is the one line a warn-strictness policy miss reaches the
// writer as.
func policyWarning(result publishpolicy.Result, policy publishpolicy.Policy) string {
	return "publish policy (warn): " + strings.Join(violationLines(result, policy), ", ")
}

// documentGetter is the one read Current needs; a view, a reader lent under
// a commit and a lock-free CurrentReader all provide it.
type documentGetter interface {
	Get(ctx context.Context, reqPath string, version int) (*storefmt.Document, error)
}

// Current loads and validates the policy document from reader. With require
// false a world without one has the empty policy.
func Current(ctx context.Context, reader documentGetter, require bool) (publishpolicy.Policy, error) {
	document, found, err := currentDocument(ctx, reader, publishpolicy.DocumentPath)
	switch {
	case err != nil:
		return publishpolicy.Policy{}, fmt.Errorf("load current policy: %w", err)
	case !found:
		if require {
			return publishpolicy.Policy{}, fmt.Errorf("%w: %w: %s", ErrInvalidPolicy, errPolicyMissing, publishpolicy.DocumentPath)
		}
		return publishpolicy.Policy{}, nil
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

// currentDocument is the current version of a well-known document through
// reader; found is false when there is none.
func currentDocument(ctx context.Context, reader documentGetter, path string) (document *storefmt.Document, found bool, err error) {
	document, err = reader.Get(ctx, path, 0)
	if errors.Is(err, backend.ErrNotFound) {
		return nil, false, nil
	}
	return document, err == nil, err
}

// evaluate judges one prepared write: the body first, then the metadata
// under the floor merged with this world's policy, a candidate policy last.
// The notes are what a warn verdict let through, then drift; none on a refusal.
func (e *enforcer) evaluate(ctx context.Context, state backend.Reader, write storefmt.PreparedWrite, d drift) ([]string, error) {
	notes, err := e.content.judge(write)
	if err != nil {
		return nil, err
	}
	var floor *publishpolicy.Policy
	if e.options.Floor != nil {
		read, err := e.options.Floor(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: read floor policy: %w", ErrInvalidPolicy, err)
		}
		floor = &read
	}
	if write.Path == publishpolicy.DocumentPath {
		candidate := publishpolicy.Parse(string(write.Content))
		if err := candidate.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %w: candidate policy: %v", backend.ErrRejected, ErrInvalidPolicy, err)
		}
		if err := e.floorSatisfiable(ctx, candidate); err != nil {
			return nil, err
		}
		// A world list disjoint from the floor's would refuse every value on
		// that axis; refuse the candidate instead.
		if floor != nil {
			if err := publishpolicy.Merge(*floor, candidate).Validate(); err != nil {
				return nil, fmt.Errorf("%w: %w: candidate policy merged under the floor: %v", backend.ErrRejected, ErrInvalidPolicy, err)
			}
		}
	}
	// A template with a bad layout line would silently route writes to the
	// default directories; refuse it the way a bad policy is refused.
	if write.Path == layout.DocumentPath {
		if _, err := layout.Parse(string(write.Content)); err != nil {
			return nil, fmt.Errorf("%w: %w: candidate template: %v", backend.ErrRejected, ErrInvalidPolicy, err)
		}
	}
	own, err := Current(ctx, state, e.options.Require)
	if err != nil {
		return nil, err
	}
	policy := publishpolicy.Effective(floor, own, write.Path)
	result := publishpolicy.EvaluateStrings(policy, write.Path, write.Metadata)
	strictness := policy.EffectiveStrictness()
	switch {
	case result.Compliant():
		return append(notes, d.notes(ctx, state, write)...), nil
	case strictness == publishpolicy.Warn:
		return slices.Concat(notes, []string{policyWarning(result, policy)}, d.notes(ctx, state, write)), nil
	}
	return nil, &PolicyError{Strictness: strictness, Result: result, Policy: policy}
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
