package semver

import "testing"

func TestParseAndLess(t *testing.T) {
	tests := []struct {
		name     string
		a, b     string
		wantLess bool
		wantOK   bool
	}{
		{name: "patch behind", a: "0.35.0", b: "0.35.1", wantLess: true, wantOK: true},
		{name: "minor behind", a: "0.17.19", b: "0.35.0", wantLess: true, wantOK: true},
		{name: "major behind", a: "0.99.99", b: "1.0.0", wantLess: true, wantOK: true},
		{name: "equal", a: "0.35.0", b: "0.35.0", wantOK: true},
		{name: "ahead", a: "0.36.0", b: "0.35.0", wantOK: true},
		{name: "numeric not lexical", a: "0.9.0", b: "0.10.0", wantLess: true, wantOK: true},
		{name: "dev build", a: "dev", b: "0.35.0"},
		{name: "tagged build", a: "0.35.0-rc1", b: "0.35.0"},
		{name: "empty probe", a: "", b: "0.35.0"},
		{name: "beyond int range", a: "99999999999999999999.0.0", b: "0.35.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, okA := Parse(tt.a)
			b, okB := Parse(tt.b)
			if ok := okA && okB; ok != tt.wantOK {
				t.Fatalf("Parse(%q, %q) ok = %v, want %v", tt.a, tt.b, ok, tt.wantOK)
			}
			if less := a.Less(b); tt.wantOK && less != tt.wantLess {
				t.Errorf("%q < %q = %v, want %v", tt.a, tt.b, less, tt.wantLess)
			}
		})
	}
}
