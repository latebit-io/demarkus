package publishpolicy

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"testing"
)

func TestEvaluateTags(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
		want     []Violation
	}{
		{name: "nil metadata", want: []Violation{{Code: MissingTags}}},
		{name: "missing", metadata: map[string]any{}, want: []Violation{{Code: MissingTags}}},
		{name: "nil", metadata: map[string]any{"tags": nil}, want: []Violation{{Code: MissingTags}}},
		{name: "blank", metadata: map[string]any{"tags": " \t\n"}, want: []Violation{{Code: MissingTags}}},
		{name: "non-string", metadata: map[string]any{"tags": []any{"topic"}}, want: []Violation{{Code: MissingTags}}},
		{name: "nonblank", metadata: map[string]any{"tags": "topic"}},
		{name: "only separators", metadata: map[string]any{"tags": ", ,"}, want: []Violation{{Code: MissingTags}}},
		{name: "nonblank token", metadata: map[string]any{"tags": ",topic,"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertViolations(t, Evaluate(Policy{}, "/doc.md", tt.metadata), tt.want)
		})
	}
}

func TestEvaluateImportance(t *testing.T) {
	tests := []struct {
		name    string
		present bool
		value   any
		valid   bool
	}{
		{name: "absent", valid: true},
		{name: "null is absent", present: true, value: nil, valid: true},
		{name: "float zero", present: true, value: float64(0), valid: true},
		{name: "float one", present: true, value: float64(1), valid: true},
		{name: "float midpoint", present: true, value: 0.5, valid: true},
		{name: "float below range", present: true, value: math.Nextafter(0, -1)},
		{name: "float above range", present: true, value: math.Nextafter(1, 2)},
		{name: "float NaN", present: true, value: math.NaN()},
		{name: "float positive infinity", present: true, value: math.Inf(1)},
		{name: "float negative infinity", present: true, value: math.Inf(-1)},
		{name: "JSON number zero", present: true, value: json.Number("0"), valid: true},
		{name: "JSON number one", present: true, value: json.Number("1"), valid: true},
		{name: "JSON number exponent", present: true, value: json.Number("5e-1"), valid: true},
		{name: "JSON number below range", present: true, value: json.Number("-0.1")},
		{name: "JSON number above range", present: true, value: json.Number("1.1")},
		{name: "JSON number invalid", present: true, value: json.Number("value")},
		{name: "JSON number NaN", present: true, value: json.Number("NaN")},
		{name: "JSON number infinity", present: true, value: json.Number("+Inf")},
		{name: "numeric string zero", present: true, value: "0", valid: true},
		{name: "numeric string one", present: true, value: "1", valid: true},
		{name: "numeric string trimmed", present: true, value: " \t0.25\n", valid: true},
		{name: "numeric string exponent", present: true, value: "1e0", valid: true},
		{name: "numeric string below range", present: true, value: "-0.1"},
		{name: "numeric string above range", present: true, value: "1.1"},
		{name: "numeric string blank", present: true, value: " \t"},
		{name: "numeric string invalid", present: true, value: "value"},
		{name: "numeric string NaN", present: true, value: "NaN"},
		{name: "numeric string infinity", present: true, value: "+Inf"},
		{name: "int rejected", present: true, value: 0},
		{name: "float32 rejected", present: true, value: float32(0.5)},
		{name: "bool rejected", present: true, value: false},
		{name: "array rejected", present: true, value: []any{}},
		{name: "object rejected", present: true, value: map[string]any{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := map[string]any{"tags": "topic"}
			if tt.present {
				metadata["importance"] = tt.value
			}
			result := Evaluate(Policy{}, "/doc.md", metadata)
			if got := !result.Has(InvalidImportance); got != tt.valid {
				t.Fatalf("importance valid = %v, want %v; violations = %#v", got, tt.valid, result.Violations)
			}
		})
	}
}

func TestEvaluateRequiredTagAxes(t *testing.T) {
	tests := []struct {
		name string
		tags any
		axes []string
		want []Violation
	}{
		{
			name: "case-sensitive axes with values",
			tags: "category:project, Team:value,empty:value",
			axes: []string{"category", "Team", "empty"},
		},
		{
			name: "empty axis value",
			tags: "category:  ",
			axes: []string{"category"},
			want: []Violation{{Code: MissingTagAxis, Name: "category"}},
		},
		{
			name: "case mismatch",
			tags: "Category:value",
			axes: []string{"category"},
			want: []Violation{{Code: MissingTagAxis, Name: "category"}},
		},
		{
			name: "axis prefix must be exact",
			tags: "categoryish:value",
			axes: []string{"category"},
			want: []Violation{{Code: MissingTagAxis, Name: "category"}},
		},
		{
			name: "tags split only on commas",
			tags: "category:value team:value",
			axes: []string{"category", "team"},
			want: []Violation{{Code: MissingTagAxis, Name: "team"}},
		},
		{
			name: "missing axes retain policy order and duplicates",
			tags: "topic",
			axes: []string{"z", "a", "z"},
			want: []Violation{
				{Code: MissingTagAxis, Name: "z"},
				{Code: MissingTagAxis, Name: "a"},
				{Code: MissingTagAxis, Name: "z"},
			},
		},
		{
			name: "absent tags suppress axis violations",
			axes: []string{"category", "team"},
			want: []Violation{{Code: MissingTags}},
		},
		{
			name: "blank tags suppress axis violations",
			tags: " ",
			axes: []string{"category"},
			want: []Violation{{Code: MissingTags}},
		},
		{
			name: "non-string tags suppress axis violations",
			tags: []any{"category:value"},
			axes: []string{"category"},
			want: []Violation{{Code: MissingTags}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := map[string]any{}
			if tt.tags != nil {
				metadata["tags"] = tt.tags
			}
			assertViolations(t, Evaluate(Policy{RequiredTagAxes: tt.axes}, "/doc.md", metadata), tt.want)
		})
	}
}

func TestEvaluateRequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		present bool
		value   any
		missing bool
	}{
		{name: "absent", missing: true},
		{name: "nil", present: true, value: nil, missing: true},
		{name: "blank string", present: true, value: " \t", missing: true},
		{name: "nonblank string", present: true, value: "value"},
		{name: "empty array", present: true, value: []any{}, missing: true},
		{name: "nonempty array", present: true, value: []any{nil}},
		{name: "empty object", present: true, value: map[string]any{}, missing: true},
		{name: "nonempty object", present: true, value: map[string]any{"key": nil}},
		{name: "zero number", present: true, value: float64(0)},
		{name: "integer number", present: true, value: 0},
		{name: "unsigned number", present: true, value: uint(0)},
		{name: "float32 number", present: true, value: float32(0)},
		{name: "false bool", present: true, value: false},
		{name: "JSON number", present: true, value: json.Number("0")},
		{name: "other types retain plugin presence semantics", present: true, value: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := map[string]any{"tags": "topic"}
			if tt.present {
				metadata["owner"] = tt.value
			}
			result := Evaluate(Policy{RequiredFields: []string{"owner"}}, "/doc.md", metadata)
			if got := result.Has(MissingField); got != tt.missing {
				t.Fatalf("missing field = %v, want %v; violations = %#v", got, tt.missing, result.Violations)
			}
		})
	}
}

func TestViolationCodeValues(t *testing.T) {
	tests := []struct {
		name  string
		code  ViolationCode
		value string
	}{
		{name: "missing tags", code: MissingTags, value: "missing-tags"},
		{name: "invalid importance", code: InvalidImportance, value: "invalid-importance"},
		{name: "missing tag axis", code: MissingTagAxis, value: "missing-tag-axis"},
		{name: "missing field", code: MissingField, value: "missing-field"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(tt.code); got != tt.value {
				t.Fatalf("violation code = %q, want %q", got, tt.value)
			}
		})
	}
}

func TestEvaluateRequiredFieldsPreserveNames(t *testing.T) {
	policy := Policy{RequiredFields: []string{"Owner", "owner", "Owner"}}
	metadata := map[string]any{"tags": "topic", "owner": "ada"}
	want := []Violation{
		{Code: MissingField, Name: "Owner"},
		{Code: MissingField, Name: "Owner"},
	}
	assertViolations(t, Evaluate(policy, "/doc.md", metadata), want)
}

func TestEvaluateNavigationTypeExemption(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		fields []string
		want   []Violation
	}{
		{name: "root index", path: "/index.md", fields: []string{"type"}},
		{name: "nested index", path: "/world/index.md", fields: []string{"type"}},
		{name: "bare index", path: "index.md", fields: []string{"type"}},
		{name: "Mark URL index", path: "mark://world/index.md", fields: []string{"type"}},
		{name: "nested log", path: "/world/log.md", fields: []string{"type"}},
		{
			name:   "only type is exempt",
			path:   "/world/index.md",
			fields: []string{"type", "owner"},
			want:   []Violation{{Code: MissingField, Name: "owner"}},
		},
		{
			name:   "type match is case-sensitive",
			path:   "/world/index.md",
			fields: []string{"Type"},
			want:   []Violation{{Code: MissingField, Name: "Type"}},
		},
		{
			name:   "basename match is case-sensitive",
			path:   "/world/Index.md",
			fields: []string{"type"},
			want:   []Violation{{Code: MissingField, Name: "type"}},
		},
		{
			name:   "trailing slash is not a document basename",
			path:   "/world/index.md/",
			fields: []string{"type"},
			want:   []Violation{{Code: MissingField, Name: "type"}},
		},
		{
			name:   "query suffix prevents exact match",
			path:   "/world/index.md?version=1",
			fields: []string{"type"},
			want:   []Violation{{Code: MissingField, Name: "type"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := map[string]any{"tags": "topic"}
			assertViolations(t, Evaluate(Policy{RequiredFields: tt.fields}, tt.path, metadata), tt.want)
		})
	}
}

func TestEvaluateViolationOrder(t *testing.T) {
	t.Run("tagless suppresses axes but keeps baseline and fields ordered", func(t *testing.T) {
		policy := Policy{
			RequiredTagAxes: []string{"z", "a"},
			RequiredFields:  []string{"owner", "type"},
		}
		metadata := map[string]any{"importance": 2.0}
		want := []Violation{
			{Code: MissingTags},
			{Code: InvalidImportance},
			{Code: MissingField, Name: "owner"},
			{Code: MissingField, Name: "type"},
		}
		assertViolations(t, Evaluate(policy, "/doc.md", metadata), want)
	})

	t.Run("importance axes and fields follow fixed order", func(t *testing.T) {
		policy := Policy{
			RequiredTagAxes: []string{"z", "a", "z"},
			RequiredFields:  []string{"type", "owner", "type"},
		}
		metadata := map[string]any{"tags": "topic", "importance": "bad"}
		want := []Violation{
			{Code: InvalidImportance},
			{Code: MissingTagAxis, Name: "z"},
			{Code: MissingTagAxis, Name: "a"},
			{Code: MissingTagAxis, Name: "z"},
			{Code: MissingField, Name: "type"},
			{Code: MissingField, Name: "owner"},
			{Code: MissingField, Name: "type"},
		}
		assertViolations(t, Evaluate(policy, "/doc.md", metadata), want)
	})
}

func TestEvaluateOverrides(t *testing.T) {
	policy := Policy{RequiredTagAxes: []string{"category"}, RequiredFields: []string{"type", "owner"}}
	tests := []struct {
		name     string
		metadata map[string]any
		want     []Violation
	}{
		{name: "absent values inherit"},
		{name: "valid overrides", metadata: map[string]any{"tags": "category:project", "type": "Reference"}},
		{name: "blank tags", metadata: map[string]any{"tags": ""}, want: []Violation{{Code: MissingTags}}},
		{name: "missing axis", metadata: map[string]any{"tags": "topic"}, want: []Violation{{Code: MissingTagAxis, Name: "category"}}},
		{name: "invalid importance", metadata: map[string]any{"importance": "bad"}, want: []Violation{{Code: InvalidImportance}}},
		{name: "blank supplied field", metadata: map[string]any{"owner": ""}, want: []Violation{{Code: MissingField, Name: "owner"}}},
		{name: "unsupplied fields remain unknown", metadata: map[string]any{"owner": "ada"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertViolations(t, EvaluateOverrides(policy, "/doc.md", tt.metadata), tt.want)
		})
	}
}

func TestResultHelpers(t *testing.T) {
	t.Run("compliant result", func(t *testing.T) {
		result := Result{}
		if !result.Compliant() {
			t.Fatal("empty result is not compliant")
		}
		if result.Has(MissingTags) {
			t.Fatal("empty result reports missing tags")
		}
		if names := result.Names(MissingField); len(names) != 0 {
			t.Fatalf("Names() = %v, want empty", names)
		}
	})

	t.Run("noncompliant result", func(t *testing.T) {
		result := Result{Violations: []Violation{
			{Code: MissingTagAxis, Name: "z"},
			{Code: MissingField, Name: "owner"},
			{Code: MissingTagAxis, Name: "a"},
			{Code: MissingTagAxis, Name: "z"},
		}}
		if result.Compliant() {
			t.Fatal("result with violations is compliant")
		}
		if !result.Has(MissingTagAxis) || result.Has(InvalidImportance) {
			t.Fatalf("Has() returned unexpected values for %#v", result.Violations)
		}
		names := result.Names(MissingTagAxis)
		if want := []string{"z", "a", "z"}; !slices.Equal(names, want) {
			t.Fatalf("Names() = %v, want %v", names, want)
		}
		names[0] = "changed"
		if result.Violations[0].Name != "z" {
			t.Fatal("Names() aliases Result.Violations")
		}
	})
}

func TestEvaluateDoesNotMutateInputs(t *testing.T) {
	policy := Policy{
		Strictness:      Block,
		RequiredTagAxes: []string{"category", "team"},
		RequiredFields:  []string{"type", "authors"},
	}
	metadata := map[string]any{
		"tags":       "topic",
		"importance": json.Number("0.5"),
		"authors":    []any{"ada"},
		"details":    map[string]any{"reviewed": true},
	}
	policyBefore := Policy{
		Strictness:      policy.Strictness,
		RequiredTagAxes: slices.Clone(policy.RequiredTagAxes),
		RequiredFields:  slices.Clone(policy.RequiredFields),
	}
	metadataBefore := map[string]any{
		"tags":       metadata["tags"],
		"importance": metadata["importance"],
		"authors":    slices.Clone(metadata["authors"].([]any)),
		"details":    map[string]any{"reviewed": true},
	}

	_ = Evaluate(policy, "/doc.md", metadata)

	if !reflect.DeepEqual(policy, policyBefore) {
		t.Fatalf("policy mutated: got %#v, want %#v", policy, policyBefore)
	}
	if !reflect.DeepEqual(metadata, metadataBefore) {
		t.Fatalf("metadata mutated: got %#v, want %#v", metadata, metadataBefore)
	}
}

func assertViolations(t *testing.T, result Result, want []Violation) {
	t.Helper()
	if !reflect.DeepEqual(result.Violations, want) {
		t.Fatalf("violations = %#v, want %#v", result.Violations, want)
	}
}
