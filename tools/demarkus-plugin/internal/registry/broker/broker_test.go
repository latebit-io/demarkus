package broker

import (
	"strings"
	"testing"
)

// Query and fragment components corrupt the appended discovery and
// /mcp paths; both are rejected before any request.
func TestValidateBrokerEndpointRejectsQueryAndFragment(t *testing.T) {
	t.Setenv("DEMARKUS_KNOWLEDGE_JOIN_ALLOW_HTTP", "1")
	for _, u := range []string{
		"http://broker.example?x=1",
		"http://broker.example/?",
		"http://broker.example/#x",
		// Empty fragment marker: Parse erases it but keeps the path
		// untrimmed, which would double the slash in appended paths.
		"http://broker.example/#",
	} {
		if _, err := Validate(u); err == nil || !strings.Contains(err.Error(), "query or fragment") {
			t.Errorf("Validate(%q) err = %v, want query/fragment rejection", u, err)
		}
	}
}
