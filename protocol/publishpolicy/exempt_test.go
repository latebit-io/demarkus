package publishpolicy

import (
	"slices"
	"testing"
)

func TestParseExempt(t *testing.T) {
	got := Parse("exempt: /graph/**, /.well-known/library/**\n").Exempt
	if want := []string{"/graph/**", "/.well-known/library/**"}; !slices.Equal(got, want) {
		t.Fatalf("Exempt = %q, want %q", got, want)
	}
}

func TestValidateExempt(t *testing.T) {
	for _, pattern := range []string{"graph/**", "/a/**/b/**", "/a/[", "/a**"} {
		if err := (Policy{Exempt: []string{pattern}}).Validate(); err == nil {
			t.Errorf("Validate accepted exempt pattern %q", pattern)
		}
	}
	if err := (Policy{Exempt: []string{"/graph/**", "/index.md", "/*/log.md"}}).Validate(); err != nil {
		t.Errorf("Validate refused valid patterns: %v", err)
	}
}

func TestAtDropsDirectivesOnExemptPaths(t *testing.T) {
	p := Policy{Strictness: Block, RequiredFields: []string{"user"}, RequiredTagAxes: []string{"category"}, Exempt: []string{"/graph/**"}}
	if got := p.at("/graph/world.md"); got.Strictness != Block || got.RequiredFields != nil || got.RequiredTagAxes != nil {
		t.Errorf("at(exempt) = %+v, want strictness only", got)
	}
	if got := p.at("/notes/a.md"); !slices.Equal(got.RequiredFields, p.RequiredFields) {
		t.Errorf("at(other) dropped rules: %+v", got)
	}
	// The baseline still judges an exempt path.
	if Evaluate(p.at("/graph/world.md"), "/graph/world.md", map[string]any{}).Compliant() {
		t.Error("an untagged exempt document is compliant")
	}
}

func TestEffectiveExemptsEachLayerFromItsOwnRules(t *testing.T) {
	floor := Policy{Strictness: Block, RequiredFields: []string{"user"}, Exempt: []string{"/graph/**"}}
	world := Policy{RequiredFields: []string{"description"}, Exempt: []string{"/hubs/**"}}
	tests := []struct {
		path string
		want []string
	}{
		{"/notes/a.md", []string{"user", "description"}},
		{"/graph/x.md", []string{"description"}},
		// A world's exemption never lifts a floor rule.
		{"/hubs/x.md", []string{"user"}},
	}
	for _, tt := range tests {
		got := Effective(&floor, world, tt.path)
		if !slices.Equal(got.RequiredFields, tt.want) || got.Strictness != Block {
			t.Errorf("Effective(%s) = %+v, want fields %q under block", tt.path, got, tt.want)
		}
	}
	if got := Effective(nil, world, "/hubs/x.md"); got.RequiredFields != nil {
		t.Errorf("Effective without a floor kept exempt rules: %+v", got)
	}
}
