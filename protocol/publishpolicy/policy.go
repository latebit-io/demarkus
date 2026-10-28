package publishpolicy

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/directives"
)

// SeedAgent is the agent metadata value on a policy a knowledge server seeded
// itself, so a provisioner can tell it from a curated policy and replace it.
const SeedAgent = "demarkus-knowledge-server"

// DocumentPath is the conventional versioned policy document path.
const DocumentPath = "/.well-known/demarkus/policy.md"

// Strictness controls how a policy violation is enforced.
type Strictness string

const (
	// Warn permits the operation while reporting policy violations.
	Warn Strictness = "warn"
	// Block rejects operations with policy violations.
	Block Strictness = "block"
	// Ask requires approval before an operation with policy violations.
	Ask Strictness = "ask"
)

// Policy is the mechanically enforceable part of a policy document.
type Policy struct {
	Strictness      Strictness
	RequiredTagAxes []string
	RequiredFields  []string
	// AllowedTagValues lists, per axis, the values an `axis:value` tag may
	// carry; an axis absent from the map is free-form. One directive line
	// per axis: `allow_tags.<axis>: v1|v2`.
	AllowedTagValues map[string][]string
	// Exempt lists the path patterns (token path syntax) this policy's
	// directives skip: generated output, configuration. Merge never carries
	// it, so a world's exemption never lifts a floor rule; see Effective.
	Exempt []string
}

// allowTagsPrefix starts the per-axis allowed-values directive key.
const allowTagsPrefix = "allow_tags."

// Parse extracts the first occurrence of each supported policy directive.
func Parse(body string) Policy {
	found := directives.First(body)
	return Policy{
		Strictness:       Strictness(found["strictness"]),
		RequiredTagAxes:  splitList(found["require_tags"]),
		RequiredFields:   splitList(found["require_fields"]),
		AllowedTagValues: allowedValues(found),
		Exempt:           splitList(found["exempt"]),
	}
}

// allowedValues collects every `allow_tags.<axis>` directive. A blank list
// stays in the map as nil, so Validate refuses an axis that allows nothing
// instead of treating it as free-form.
func allowedValues(found map[string]string) map[string][]string {
	var values map[string][]string
	for key, raw := range found {
		axis, ok := strings.CutPrefix(key, allowTagsPrefix)
		if !ok {
			continue
		}
		if values == nil {
			values = make(map[string][]string)
		}
		values[axis] = splitOn(raw, '|')
	}
	return values
}

func splitList(value string) []string { return splitOn(value, ',') }

// splitOn splits on sep and whitespace, dropping empties; nil when none.
func splitOn(value string, sep rune) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == sep || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// AllowedList words an axis's allowed values as the directive writes them.
func (p Policy) AllowedList(axis string) string {
	return strings.Join(p.AllowedTagValues[axis], "|")
}

// Validate rejects unsupported non-empty strictness values.
func (p Policy) Validate() error {
	if p.Strictness != "" && p.Strictness != Warn && p.Strictness != Block && p.Strictness != Ask {
		return fmt.Errorf("invalid publish policy strictness %q", p.Strictness)
	}
	if err := validateAxes(p.RequiredTagAxes); err != nil {
		return err
	}
	if err := validateFields(p.RequiredFields); err != nil {
		return err
	}
	if err := validateAllowedValues(p.AllowedTagValues); err != nil {
		return err
	}
	if err := validateExempt(p.Exempt); err != nil {
		return err
	}
	return validateSatisfiable(p)
}

// validateExempt holds each pattern to the token path syntax, rooted.
func validateExempt(patterns []string) error {
	if len(patterns) > maxExemptPatterns {
		return fmt.Errorf("publish policy has too many exempt patterns: %d > %d", len(patterns), maxExemptPatterns)
	}
	for _, pattern := range patterns {
		if !strings.HasPrefix(pattern, "/") {
			return fmt.Errorf("exempt pattern %q is not rooted at /", pattern)
		}
		if err := protocol.ValidatePathPattern(pattern); err != nil {
			return fmt.Errorf("exempt pattern %q: %w", pattern, err)
		}
		// MatchesAnyPath reads the part before ** literally, so glob syntax
		// there would not mean what it says; a pattern without ** escapes.
		if before, _, ok := strings.Cut(pattern, "**"); ok && strings.ContainsAny(before, `*?[\`) {
			return fmt.Errorf(`exempt pattern %q: the part before ** is literal, so * ? [ \ are refused there`, pattern)
		}
	}
	return nil
}

// at is the policy as it applies at documentPath: an exempt path keeps
// only the strictness, so the baseline checks still decide its fate.
func (p Policy) at(documentPath string) Policy {
	if protocol.MatchesAnyPath(p.Exempt, documentPath) {
		return Policy{Strictness: p.Strictness}
	}
	return p
}

// Effective is the policy a write at documentPath is held to: each layer's
// own exemptions first, then the world merged under the floor; nil floor
// means none.
func Effective(floor *Policy, world Policy, documentPath string) Policy {
	world = world.at(documentPath)
	if floor == nil {
		return world
	}
	return Merge(floor.at(documentPath), world)
}

// validateAllowedValues holds each allow_tags axis to the axis rules and
// each value to the tag rules; an axis allowing nothing would refuse every
// tag on it, so it is refused instead.
func validateAllowedValues(values map[string][]string) error {
	if len(values) > maxRequiredAxes {
		return fmt.Errorf("publish policy has too many allow_tags axes: %d > %d", len(values), maxRequiredAxes)
	}
	for _, axis := range slices.Sorted(maps.Keys(values)) {
		if err := validateNames("tag axis", []string{axis}, ":,"); err != nil {
			return fmt.Errorf("allow_tags.%s: %w", axis, err)
		}
		if len(values[axis]) == 0 {
			return fmt.Errorf("allow_tags.%s allows no values", axis)
		}
		if len(values[axis]) > maxAllowedValues {
			return fmt.Errorf("allow_tags.%s has too many values: %d > %d", axis, len(values[axis]), maxAllowedValues)
		}
		if err := validateNames("value", values[axis], ",|"); err != nil {
			return fmt.Errorf("allow_tags.%s: %w", axis, err)
		}
	}
	return nil
}

// ForCaller is the policy a pre-write gate holds a caller to: required
// identity keys are dropped, since the surface stamps them after the gate.
func (p Policy) ForCaller() Policy {
	p.RequiredFields = slices.DeleteFunc(slices.Clone(p.RequiredFields), protocol.IsIdentityMetadataKey)
	return p
}

// EffectiveStrictness returns Warn for absent or invalid strictness values.
func (p Policy) EffectiveStrictness() Strictness {
	switch p.Strictness {
	case Warn, Block, Ask:
		return p.Strictness
	default:
		return Warn
	}
}

const (
	maxRequiredAxes    = protocol.MaxMetaKeys
	maxRequiredFields  = protocol.MaxMetaKeys
	maxAllowedValues   = 64
	maxExemptPatterns  = 64
	maxPolicyNameBytes = 64
)

func validateAxes(axes []string) error {
	if len(axes) > maxRequiredAxes {
		return fmt.Errorf("publish policy has too many required tag axes: %d > %d", len(axes), maxRequiredAxes)
	}
	return validateNames("required tag axis", axes, ":,")
}

// validateNames holds tag axes and allowed values to one rule: non-empty,
// bounded, valid UTF-8, no whitespace or control runes, none of forbidden,
// no duplicates.
func validateNames(what string, names []string, forbidden string) error {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if name == "" || len(name) > maxPolicyNameBytes || !utf8.ValidString(name) || strings.ContainsAny(name, forbidden) {
			return fmt.Errorf("invalid %s %q", what, name)
		}
		for _, r := range name {
			if unicode.IsSpace(r) || unicode.IsControl(r) {
				return fmt.Errorf("invalid %s %q", what, name)
			}
		}
		if seen[name] {
			return fmt.Errorf("duplicate %s %q", what, name)
		}
		seen[name] = true
	}
	return nil
}

var unavailableFields = map[string]bool{
	"archived":          true,
	"auth":              true,
	"chain-error":       true,
	"chain-valid":       true,
	"content-hash":      true,
	"current":           true,
	"current-version":   true,
	"entries":           true,
	"etag":              true,
	"expected-version":  true,
	"if-modified-since": true,
	"if-none-match":     true,
	"matches":           true,
	"modified":          true,
	"policy-warning":    true,
	"previous-hash":     true,
	"server-version":    true,
	"status":            true,
	"total":             true,
	"version":           true,
	"your-version":      true,
}

func validateFields(fields []string) error {
	if len(fields) > maxRequiredFields {
		return fmt.Errorf("publish policy has too many required fields: %d > %d", len(fields), maxRequiredFields)
	}
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		if !validFieldName(field) || unavailableFields[field] {
			return fmt.Errorf("invalid required metadata field %q", field)
		}
		if seen[field] {
			return fmt.Errorf("duplicate required metadata field %q", field)
		}
		seen[field] = true
	}
	return nil
}

func validFieldName(field string) bool {
	if field == "" || len(field) > maxPolicyNameBytes {
		return false
	}
	for _, r := range field {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func validateSatisfiable(policy Policy) error {
	metadata := minimumMetadata(policy)
	if len(metadata) > protocol.MaxMetaKeys {
		return fmt.Errorf("publish policy needs at least %d metadata keys; maximum is %d", len(metadata), protocol.MaxMetaKeys)
	}
	size := 0
	for key, value := range metadata {
		if key == "tags" {
			value = "[" + strings.ReplaceAll(value, ",", ", ") + "]"
		}
		size += len(key) + len(value)
	}
	if size > protocol.MaxMetaBytes {
		return fmt.Errorf("publish policy needs at least %d metadata bytes; maximum is %d", size, protocol.MaxMetaBytes)
	}
	return nil
}

func minimumMetadata(policy Policy) map[string]string {
	metadata := make(map[string]string, len(policy.RequiredFields)+2)
	tags := "policy"
	if len(policy.RequiredTagAxes) > 0 {
		values := make([]string, len(policy.RequiredTagAxes))
		for i, axis := range policy.RequiredTagAxes {
			values[i] = axis + ":x"
		}
		tags = strings.Join(values, ",")
	}
	metadata["tags"] = tags
	for _, field := range policy.RequiredFields {
		switch field {
		case "tags":
			continue
		case "importance":
			metadata[field] = "0"
		case "retention":
			metadata[field] = "1"
		default:
			metadata[field] = "x"
		}
	}
	if _, present := metadata["type"]; !present {
		metadata["type"] = protocol.OKFDefaultType
	}
	return metadata
}
