package broker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// Reach answers nil on the public metadata, names the status otherwise,
// and words an expired deadline apart from an unreachable host.
func TestReach(t *testing.T) {
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	if err := Reach(context.Background(), srv.URL); err != nil {
		t.Fatalf("200: %v", err)
	}
	status = 404
	if err := Reach(context.Background(), srv.URL); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("404: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if err := Reach(ctx, srv.URL); err == nil || !strings.Contains(err.Error(), "within the deadline") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	if err := Reach(context.Background(), "http://127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("refused: %v", err)
	}
}
