package protocol

import "testing"

func TestParseFlag(t *testing.T) {
	for value, want := range map[string]bool{"": false, "false": false, "true": true} {
		if got, err := ParseFlag("pins", value); err != nil || got != want {
			t.Errorf("ParseFlag(%q) = %v, %v; want %v", value, got, err, want)
		}
	}
	for _, bad := range []string{"TRUE", "yes", "1"} {
		if _, err := ParseFlag("pins", bad); err == nil || err.Error() != "pins must be true or false" {
			t.Errorf("ParseFlag(%q) = %v, want refused", bad, err)
		}
	}
}
