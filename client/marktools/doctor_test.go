package marktools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/render"
)

// The audit itself is tested in client/audit; here that the tool scopes it
// to the resolved target, reads with the read token, and words the outcome.
func TestDoctorAuditsTheScopeThroughTheBackend(t *testing.T) {
	docs := map[string]string{
		"/index.md":  "# Hub\n\n- [a](/a.md)\n",
		"/a.md":      "# A\n\nWhat A settles for readers.\n",
		"/orphan.md": "No heading — and nobody links here.\n",
	}
	backend := &fetchtest.Client{
		ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			if r.Host != "host:6309" || r.Token != "token-for-host:6309" {
				t.Errorf("list sent to %s with token %q", r.Host, r.Token)
			}
			entries := []render.ListEntry{{Name: "a.md"}, {Name: "index.md"}, {Name: "orphan.md"}}
			return fetch.Result{Response: render.ListResponse("/", entries, "")}, nil
		},
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			body, ok := docs[r.Path]
			if !ok {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			return fetchtest.Head(body, 1, map[string]string{"tags": "t", "type": "Guide"}), nil
		},
	}
	tools := newTools(t, backend, directHooks())
	got := tools.Doctor(t.Context(), marktools.DoctorArgs{URL: "/"})
	if got.IsError {
		t.Fatalf("doctor = %+v", got)
	}
	for _, want := range []string{"## / hygiene report  (3 docs)", "### Orphans (1)\n- /orphan.md:", "### Untitled (1)", "### Coverage\n- complete"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("report missing %q:\n%s", want, got.Text)
		}
	}
	if strings.Contains(got.Text, "Promotion candidates") {
		t.Error("promotion section without the flag")
	}
}

func TestDoctorRefusesWhatItCannotTrust(t *testing.T) {
	backend := &fetchtest.Client{
		ListFn: func(context.Context, fetch.ListRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusUnauthorized}}, nil
		},
	}
	tools := newTools(t, backend, directHooks())
	got := tools.Doctor(t.Context(), marktools.DoctorArgs{URL: "/proj/"})
	if !got.IsError || !strings.Contains(got.Text, "doctor failed: list /proj/ returned unauthorized") {
		t.Fatalf("doctor = %+v", got)
	}
	if got := tools.Doctor(t.Context(), marktools.DoctorArgs{URL: "bad"}); !got.IsError || !strings.HasPrefix(got.Text, "invalid URL") {
		t.Fatalf("bad url = %+v", got)
	}
}
