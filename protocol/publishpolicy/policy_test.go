package publishpolicy

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/store"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Policy
	}{
		{
			name: "all directives",
			body: "strictness: block\nrequire_tags: category team\nrequire_fields: type, authors\n",
			want: Policy{
				Strictness:      Block,
				RequiredTagAxes: []string{"category", "team"},
				RequiredFields:  []string{"type", "authors"},
			},
		},
		{
			name: "leading spaces and tabs",
			body: " \tstrictness:\task \t\n\trequire_tags:\tcategory\n  require_fields: type\n",
			want: Policy{Strictness: Ask, RequiredTagAxes: []string{"category"}, RequiredFields: []string{"type"}},
		},
		{
			name: "unknown prose case and other leading whitespace ignored",
			body: "STRICTNESS: block\nprose strictness: block\n\vstrictness: block\nstrictness: warn\nrequire_tags_extra: wrong\nrequire_tags : wrong\nrequire_tags: right\n",
			want: Policy{Strictness: Warn, RequiredTagAxes: []string{"right"}},
		},
		{
			name: "first occurrence of each directive wins",
			body: "require_fields: owner\nstrictness: ask\nrequire_tags: Team category\nstrictness: block\nrequire_tags: ignored\nrequire_fields: ignored\n",
			want: Policy{Strictness: Ask, RequiredTagAxes: []string{"Team", "category"}, RequiredFields: []string{"owner"}},
		},
		{
			name: "blank first occurrence suppresses later values",
			body: "strictness: \t\nstrictness: block\nrequire_tags:\nrequire_tags: category\nrequire_fields:  \nrequire_fields: type\n",
			want: Policy{},
		},
		{
			name: "lists preserve current separators case order and duplicates",
			body: "require_tags: Category,team\tteam\vstatus\fowner\r\nrequire_fields: Type,type, owner\n",
			want: Policy{
				RequiredTagAxes: []string{"Category", "team", "team\vstatus\fowner"},
				RequiredFields:  []string{"Type", "type", "owner"},
			},
		},
		{
			name: "non-ASCII whitespace remains inside a token",
			body: "require_tags: category\u00a0team,owner\n",
			want: Policy{RequiredTagAxes: []string{"category\u00a0team", "owner"}},
		},
		{
			name: "malformed strictness remains raw",
			body: "strictness: BLOCK later prose\n",
			want: Policy{Strictness: "BLOCK later prose"},
		},
		{
			name: "prefix match does not require a space",
			body: "strictness:: block\nrequire_tags:category\nrequire_fields:type\n",
			want: Policy{Strictness: ": block", RequiredTagAxes: []string{"category"}, RequiredFields: []string{"type"}},
		},
		{
			name: "no directives",
			body: "# Policy\nNothing enforceable here.\n",
			want: Policy{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Parse(tt.body); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Parse() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestPolicyValidate(t *testing.T) {
	tests := []struct {
		name    string
		value   Strictness
		wantErr bool
	}{
		{name: "absent", value: ""},
		{name: "warn", value: Warn},
		{name: "block", value: Block},
		{name: "ask", value: Ask},
		{name: "unknown", value: "allow", wantErr: true},
		{name: "uppercase", value: "WARN", wantErr: true},
		{name: "untrimmed", value: " warn ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (Policy{Strictness: tt.value}).Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPolicyValidateNames(t *testing.T) {
	tests := []struct {
		name   string
		policy Policy
		valid  bool
	}{
		{name: "valid", policy: Policy{RequiredTagAxes: []string{"category", "Team/name"}, RequiredFields: []string{"type", "owner-name"}}, valid: true},
		{name: "empty axis", policy: Policy{RequiredTagAxes: []string{""}}},
		{name: "axis colon", policy: Policy{RequiredTagAxes: []string{"category:"}}},
		{name: "axis comma", policy: Policy{RequiredTagAxes: []string{"category,team"}}},
		{name: "axis whitespace", policy: Policy{RequiredTagAxes: []string{"category team"}}},
		{name: "duplicate axis", policy: Policy{RequiredTagAxes: []string{"team", "team"}}},
		{name: "empty field", policy: Policy{RequiredFields: []string{""}}},
		{name: "uppercase field", policy: Policy{RequiredFields: []string{"Type"}}},
		{name: "punctuated field", policy: Policy{RequiredFields: []string{"owner.name"}}},
		{name: "control field", policy: Policy{RequiredFields: []string{"auth"}}},
		{name: "response field", policy: Policy{RequiredFields: []string{"version"}}},
		{name: "duplicate field", policy: Policy{RequiredFields: []string{"owner", "owner"}}},
		{name: "too many axes", policy: Policy{RequiredTagAxes: make([]string, maxRequiredAxes+1)}},
		{name: "too many fields", policy: Policy{RequiredFields: make([]string, maxRequiredFields+1)}},
		{name: "long axis", policy: Policy{RequiredTagAxes: []string{strings.Repeat("a", maxPolicyNameBytes+1)}}},
		{name: "long field", policy: Policy{RequiredFields: []string{strings.Repeat("a", maxPolicyNameBytes+1)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Validate()
			if (err == nil) != tt.valid {
				t.Fatalf("Validate() error = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

func TestPolicyEffectiveStrictness(t *testing.T) {
	tests := []struct {
		name  string
		value Strictness
		want  Strictness
	}{
		{name: "absent defaults to warn", want: Warn},
		{name: "invalid defaults to warn", value: "allow", want: Warn},
		{name: "warn unchanged", value: Warn, want: Warn},
		{name: "block unchanged", value: Block, want: Block},
		{name: "ask unchanged", value: Ask, want: Ask},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Policy{Strictness: tt.value}).EffectiveStrictness(); got != tt.want {
				t.Fatalf("EffectiveStrictness() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPolicyValidateSatisfiable(t *testing.T) {
	fields := func(count int) []string {
		values := make([]string, count)
		for i := range count {
			values[i] = fmt.Sprintf("f%d", i)
		}
		return values
	}
	if err := (Policy{RequiredFields: fields(48)}).Validate(); err != nil {
		t.Fatalf("48 custom fields should fit with tags and default type: %v", err)
	}
	if err := (Policy{RequiredFields: fields(49)}).Validate(); err == nil {
		t.Fatal("49 custom fields should exceed the metadata-key limit")
	}

	axes := make([]string, 20)
	for i := range axes {
		axes[i] = fmt.Sprintf("axis-%02d-%s", i, strings.Repeat("a", 52))
	}
	if err := (Policy{RequiredTagAxes: axes}).Validate(); err == nil {
		t.Fatal("oversized minimum tag metadata should be rejected")
	}
}

func TestValidatedMinimumMetadataPassesStoreValidation(t *testing.T) {
	policies := []Policy{
		{},
		{RequiredTagAxes: []string{"category", "team"}},
		{RequiredFields: []string{"tags", "type", "importance", "retention", "owner"}},
	}
	for _, policy := range policies {
		if err := policy.Validate(); err != nil {
			t.Fatalf("Validate(%#v): %v", policy, err)
		}
		if err := store.ValidateMeta(minimumMetadata(policy)); err != nil {
			t.Fatalf("minimum metadata for %#v: %v", policy, err)
		}
	}
}

func TestStrictnessValues(t *testing.T) {
	tests := []struct {
		name  string
		value Strictness
		want  string
	}{
		{name: "warn", value: Warn, want: "warn"},
		{name: "block", value: Block, want: "block"},
		{name: "ask", value: Ask, want: "ask"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(tt.value); got != tt.want {
				t.Fatalf("strictness value = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDocumentPath(t *testing.T) {
	if DocumentPath != "/.well-known/demarkus/policy.md" {
		t.Fatalf("DocumentPath = %q", DocumentPath)
	}
}
