package answerbench

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

func TestReadinessReportsFinalProbeReason(t *testing.T) {
	for _, tc := range []struct {
		name, status, body, want string
		failure                  error
	}{
		{"wrong-body", "ok", "another server", "body-match=false", nil},
		{"wrong-status", "not-found", "", `status="not-found"`, nil},
		{"transport", "", "", "connection refused", errors.New("connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			err := waitForIndex(ctx, "# Expected", func(context.Context) (fetch.Result, error) {
				cancel()
				return fetch.Result{Response: protocol.Response{Status: tc.status, Body: tc.body}}, tc.failure
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("probe error=%v", err)
			}
		})
	}
	if err := waitForIndex(t.Context(), "# Expected", func(context.Context) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: "ok", Body: "# Expected"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
}
