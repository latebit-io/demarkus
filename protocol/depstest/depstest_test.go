package depstest

import (
	"slices"
	"testing"
)

// The standard library links nothing forbidden, and a named prefix is caught
// once however many packages sit under it.
func TestForbidden(t *testing.T) {
	Forbid(t, "strings")
	linked, err := forbidden("strings", []string{"unicode/"})
	if err != nil || len(linked) != 1 || !slices.Contains([]string{"unicode/utf8", "unicode/utf16"}, linked[0]) {
		t.Errorf("forbidden = %v, %v; want one package under unicode/", linked, err)
	}
	if _, err := forbidden("./no-such-package", nil); err == nil {
		t.Error("a pattern go list refuses was not reported")
	}
}
