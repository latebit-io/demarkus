package publishpolicy

import (
	"reflect"
	"testing"
)

func TestParseAllowedValues(t *testing.T) {
	body := "strictness: block\nallow_tags.category: runbook|decision | guide\nallow_tags.status: active||deprecated\nallow_tags.category: ignored\nallow_tags.empty:\n"
	got := Parse(body).AllowedTagValues
	want := map[string][]string{
		"category": {"runbook", "decision", "guide"},
		"status":   {"active", "deprecated"},
		"empty":    nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowedTagValues = %v, want %v", got, want)
	}
	if Parse("# Policy\n").AllowedTagValues != nil {
		t.Error("a policy without allow_tags should carry a nil map")
	}
}

func TestValidateAllowedValues(t *testing.T) {
	tests := []struct {
		name   string
		values map[string][]string
		valid  bool
	}{
		{name: "none", valid: true},
		{name: "valid", values: map[string][]string{"category": {"runbook", "guide"}}, valid: true},
		{name: "empty list", values: map[string][]string{"category": {}}},
		{name: "bad axis", values: map[string][]string{"cat egory": {"x"}}},
		{name: "value with comma", values: map[string][]string{"category": {"a,b"}}},
		{name: "value with pipe", values: map[string][]string{"category": {"a|b"}}},
		{name: "duplicate value", values: map[string][]string{"category": {"a", "a"}}},
		{name: "too many values", values: map[string][]string{"category": make([]string, maxAllowedValues+1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (Policy{AllowedTagValues: tt.values}).Validate()
			if (err == nil) != tt.valid {
				t.Fatalf("Validate() error = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

func TestEvaluateAllowedValues(t *testing.T) {
	policy := Policy{AllowedTagValues: map[string][]string{"category": {"runbook", "guide"}}}
	tests := []struct {
		name string
		tags string
		want []Violation
	}{
		{name: "listed value", tags: "category:runbook, topic"},
		{name: "free-form axis", tags: "status:whatever"},
		{name: "unlisted value", tags: "category:howto", want: []Violation{{Code: DisallowedTagValue, Name: "category:howto", Axis: "category"}}},
		{name: "spaces around the value", tags: "category: guide"},
		{name: "two bad tags keep order", tags: "category:a,category:b", want: []Violation{
			{Code: DisallowedTagValue, Name: "category:a", Axis: "category"}, {Code: DisallowedTagValue, Name: "category:b", Axis: "category"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertViolations(t, Evaluate(policy, "/doc.md", map[string]any{"tags": tt.tags}), tt.want)
		})
	}
	// Overrides only judge tags the update supplies.
	assertViolations(t, EvaluateOverrides(policy, "/doc.md", map[string]any{"type": "Guide"}), nil)
}

func TestMerge(t *testing.T) {
	floor := Policy{
		Strictness:       Ask,
		RequiredTagAxes:  []string{"category"},
		RequiredFields:   []string{"type"},
		AllowedTagValues: map[string][]string{"category": {"a", "b", "c"}, "status": {"active"}},
	}
	world := Policy{
		Strictness:       Warn,
		RequiredTagAxes:  []string{"team", "category"},
		RequiredFields:   []string{"owner"},
		AllowedTagValues: map[string][]string{"category": {"c", "a", "z"}, "team": {"core"}},
	}
	got := Merge(floor, world)
	want := Policy{
		Strictness:       Ask,
		RequiredTagAxes:  []string{"category", "team"},
		RequiredFields:   []string{"type", "owner"},
		AllowedTagValues: map[string][]string{"category": {"a", "c"}, "status": {"active"}, "team": {"core"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge = %+v, want %+v", got, want)
	}
	if got := Merge(Policy{}, Policy{Strictness: Block}); got.Strictness != Block || got.AllowedTagValues != nil {
		t.Errorf("empty floor changed the world: %+v", got)
	}
	if got := Merge(Policy{Strictness: Block}, Policy{}); got.Strictness != Block {
		t.Errorf("empty world escaped the floor: %+v", got)
	}
	// An intersection allowing nothing is kept so Validate can refuse it.
	disjoint := Merge(Policy{AllowedTagValues: map[string][]string{"x": {"a"}}}, Policy{AllowedTagValues: map[string][]string{"x": {"b"}}})
	if disjoint.Validate() == nil {
		t.Error("disjoint allowed lists should not validate")
	}
}

func TestLoosens(t *testing.T) {
	floor := Policy{
		Strictness:       Block,
		RequiredTagAxes:  []string{"category"},
		RequiredFields:   []string{"type"},
		AllowedTagValues: map[string][]string{"category": {"a"}},
	}
	if got := Loosens(floor, Policy{Strictness: Block, RequiredTagAxes: []string{"category", "team"}, RequiredFields: []string{"type"}}); len(got) != 0 {
		t.Errorf("a tightening world loosens: %v", got)
	}
	if got := Loosens(floor, Policy{}); len(got) != 0 {
		t.Errorf("an empty world policy loosens: %v", got)
	}
	got := Loosens(floor, Policy{Strictness: Warn, AllowedTagValues: map[string][]string{"category": {"a", "b"}}})
	want := []string{
		"strictness warn is below the floor's block",
		"allow_tags.category adds b, which the floor does not allow",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Loosens = %q, want %q", got, want)
	}
}
