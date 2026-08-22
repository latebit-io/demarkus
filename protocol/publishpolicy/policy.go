package publishpolicy

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/protocol"
)

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
}

// Parse extracts the first occurrence of each supported policy directive.
func Parse(body string) Policy {
	var policy Policy
	var strictnessSet, tagAxesSet, fieldsSet bool

	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimLeft(line, " \t")
		switch {
		case !strictnessSet && strings.HasPrefix(line, "strictness:"):
			policy.Strictness = Strictness(strings.TrimSpace(line[len("strictness:"):]))
			strictnessSet = true
		case !tagAxesSet && strings.HasPrefix(line, "require_tags:"):
			policy.RequiredTagAxes = splitList(strings.TrimSpace(line[len("require_tags:"):]))
			tagAxesSet = true
		case !fieldsSet && strings.HasPrefix(line, "require_fields:"):
			policy.RequiredFields = splitList(strings.TrimSpace(line[len("require_fields:"):]))
			fieldsSet = true
		}
	}

	return policy
}

func splitList(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(fields) == 0 {
		return nil
	}
	return fields
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
	return validateSatisfiable(p)
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
	maxPolicyNameBytes = 64
)

func validateAxes(axes []string) error {
	if len(axes) > maxRequiredAxes {
		return fmt.Errorf("publish policy has too many required tag axes: %d > %d", len(axes), maxRequiredAxes)
	}
	seen := make(map[string]bool, len(axes))
	for _, axis := range axes {
		if axis == "" || len(axis) > maxPolicyNameBytes || !utf8.ValidString(axis) {
			return fmt.Errorf("invalid required tag axis %q", axis)
		}
		for _, r := range axis {
			if r == ':' || r == ',' || unicode.IsSpace(r) || unicode.IsControl(r) {
				return fmt.Errorf("invalid required tag axis %q", axis)
			}
		}
		if seen[axis] {
			return fmt.Errorf("duplicate required tag axis %q", axis)
		}
		seen[axis] = true
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
