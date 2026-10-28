package publishpolicy

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/provenance"
)

// ViolationCode identifies one stable class of policy violation.
type ViolationCode string

const (
	// MissingTags means metadata.tags is not a non-blank string.
	MissingTags ViolationCode = "missing-tags"
	// InvalidImportance means a present metadata.importance is invalid.
	InvalidImportance ViolationCode = "invalid-importance"
	// MissingTagAxis means one policy-required tag axis is absent.
	MissingTagAxis ViolationCode = "missing-tag-axis"
	// MissingField means one policy-required metadata field is absent.
	MissingField ViolationCode = "missing-field"
	// DisallowedTagValue means an `axis:value` tag carries a value outside
	// the policy's allowed list for that axis; Name is the offending tag.
	DisallowedTagValue ViolationCode = "disallowed-tag-value"
	// UnpinnedSource means a rel-based-on document path or GitHub URL names
	// no version, or a value is malformed, so its drift cannot be measured;
	// Name is the value. Any other URL counts as the writer's permalink.
	UnpinnedSource ViolationCode = "unpinned-source"
)

// Violation identifies a violation and its axis or field name, when
// applicable. Axis is set on DisallowedTagValue, whose Name is the full tag.
type Violation struct {
	Code ViolationCode
	Name string
	Axis string
}

// Result contains violations in deterministic evaluation order.
type Result struct {
	Violations []Violation
}

// Compliant reports whether evaluation found no violations.
func (r Result) Compliant() bool {
	return len(r.Violations) == 0
}

// Has reports whether evaluation found the given violation code.
func (r Result) Has(code ViolationCode) bool {
	for _, violation := range r.Violations {
		if violation.Code == code {
			return true
		}
	}
	return false
}

// Names returns names attached to violations with the given code, in order.
func (r Result) Names(code ViolationCode) []string {
	names := make([]string, 0, len(r.Violations))
	for _, violation := range r.Violations {
		if violation.Code == code {
			names = append(names, violation.Name)
		}
	}
	return names
}

// Evaluate checks metadata against the baseline and supplied policy.
func Evaluate(policy Policy, path string, metadata map[string]any) Result {
	return evaluate(policy, path, metadata, false)
}

// EvaluateStrings is Evaluate over the string metadata a store or a tool
// holds.
func EvaluateStrings(policy Policy, path string, metadata map[string]string) Result {
	values := make(map[string]any, len(metadata))
	for key, value := range metadata {
		values[key] = value
	}
	return evaluate(policy, path, values, false)
}

// EvaluateOverrides checks only fields explicitly supplied by an update.
// It catches invalid APPEND overrides without treating inherited fields as absent.
func EvaluateOverrides(policy Policy, path string, metadata map[string]any) Result {
	return evaluate(policy, path, metadata, true)
}

func evaluate(policy Policy, path string, metadata map[string]any, overridesOnly bool) Result {
	var result Result
	tags, tagsPresent := tagValue(metadata)
	_, tagsSupplied := metadata["tags"]
	if !tagsPresent && (!overridesOnly || tagsSupplied) {
		result.Violations = append(result.Violations, Violation{Code: MissingTags})
	}
	if !importanceValid(metadata) {
		result.Violations = append(result.Violations, Violation{Code: InvalidImportance})
	}
	if pins, ok := metadata[provenance.Key].(string); ok {
		for _, ref := range provenance.Unpinned(pins) {
			result.Violations = append(result.Violations, Violation{Code: UnpinnedSource, Name: ref})
		}
	}
	if tagsPresent {
		for _, axis := range policy.RequiredTagAxes {
			if !tagsHaveAxis(tags, axis) {
				result.Violations = append(result.Violations, Violation{Code: MissingTagAxis, Name: axis})
			}
		}
		result.Violations = append(result.Violations, disallowedTags(tags, policy.AllowedTagValues)...)
	}
	for _, field := range policy.RequiredFields {
		if _, supplied := metadata[field]; overridesOnly && !supplied {
			continue
		}
		if field == "type" && NavigationExempt(path) {
			continue
		}
		if !fieldPresent(metadata, field) {
			result.Violations = append(result.Violations, Violation{Code: MissingField, Name: field})
		}
	}
	return result
}

func tagValue(metadata map[string]any) (string, bool) {
	tags, ok := metadata["tags"].(string)
	if !ok {
		return "", false
	}
	return tags, len(protocol.SplitTags(tags)) > 0
}

func importanceValid(metadata map[string]any) bool {
	value, present := metadata["importance"]
	if !present || value == nil {
		return true
	}

	var number float64
	switch value := value.(type) {
	case float64:
		number = value
	case json.Number:
		parsed, err := value.Float64()
		if err != nil {
			return false
		}
		number = parsed
	case string:
		value = strings.TrimSpace(value)
		if value == "" {
			return false
		}
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return false
		}
		number = parsed
	default:
		return false
	}

	return !math.IsNaN(number) && !math.IsInf(number, 0) && number >= 0 && number <= 1
}

func tagsHaveAxis(tags, axis string) bool {
	for _, tag := range protocol.SplitTags(tags) {
		prefix := axis + ":"
		if strings.HasPrefix(tag, prefix) && strings.TrimSpace(tag[len(prefix):]) != "" {
			return true
		}
	}
	return false
}

// disallowedTags returns, in tag order, every `axis:value` tag whose axis
// has an allowed list the value is not on. A tag without a colon has no
// axis; a tag on a free-form axis is never flagged.
func disallowedTags(tags string, allowed map[string][]string) []Violation {
	if len(allowed) == 0 {
		return nil
	}
	var out []Violation
	for _, tag := range protocol.SplitTags(tags) {
		axis, value, ok := strings.Cut(tag, ":")
		if !ok {
			continue
		}
		values, listed := allowed[axis]
		if listed && !slices.Contains(values, strings.TrimSpace(value)) {
			out = append(out, Violation{Code: DisallowedTagValue, Name: tag, Axis: axis})
		}
	}
	return out
}

func fieldPresent(metadata map[string]any, field string) bool {
	value, present := metadata[field]
	if !present || value == nil {
		return false
	}
	switch value := value.(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case []any:
		return len(value) > 0
	case map[string]any:
		return len(value) > 0
	default:
		return true
	}
}

// NavigationExempt reports the OKF navigation leaves (index.md, log.md) the
// type rule skips; shared so the style gate agrees on the same leaves.
func NavigationExempt(documentPath string) bool {
	if slash := strings.LastIndex(documentPath, "/"); slash >= 0 {
		documentPath = documentPath[slash+1:]
	}
	return documentPath == "index.md" || documentPath == "log.md"
}
