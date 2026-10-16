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
	for _, want := range []string{"## / hygiene report  (3 docs)", "1 fixable: mark_curate / proposes the writes\n", "### Orphans (1, 1 fixable)\n- /orphan.md:", "### Untitled (1)", "### Coverage\n- complete"} {
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

// Behind a broker the doctor judges a world under the floor world's policy
// merged with its own; an unreadable floor fails the audit.
func TestDoctorReadsTheFloorPolicy(t *testing.T) {
	policies := map[string]string{
		"root": "# Policy\n\nstrictness: block\nrequire_tags: category\n",
		"team": "# Policy\n\nstrictness: warn\n",
	}
	backend := &fetchtest.Client{
		ListFn: func(context.Context, fetch.ListRequest) (fetch.Result, error) {
			entries := []render.ListEntry{{Name: "a.md"}, {Name: "index.md"}}
			return fetch.Result{Response: render.ListResponse("/", entries, "")}, nil
		},
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			switch r.Path {
			case "/.well-known/demarkus/policy.md":
				if body, ok := policies[r.Host]; ok {
					return fetchtest.Head(body, 1, map[string]string{"tags": "category:governance"}), nil
				}
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusServerError}}, nil
			case "/index.md":
				return fetchtest.Head("# Hub\n\n- [a](/a.md)\n", 1, map[string]string{"tags": "category:hub"}), nil
			case "/a.md":
				return fetchtest.Head("# A\n\nWhat A settles.\n", 1, map[string]string{"tags": "topic", "type": "Guide"}), nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	hooks := directHooks()
	hooks.Resolve = func(context.Context, string) (marktools.Target, error) {
		return marktools.Target{Host: "team", Path: "/"}, nil
	}
	floorHost := "root"
	hooks.Floor = func(_ context.Context, target marktools.Target) (marktools.Target, bool) {
		return marktools.Target{Host: floorHost, Path: "/"}, target.Host != floorHost
	}
	tools := newTools(t, backend, hooks)
	got := tools.Doctor(t.Context(), marktools.DoctorArgs{URL: "/"})
	if got.IsError || !strings.Contains(got.Text, "### Policy compliance (1)\n- /a.md: missing required tag axis category: (strictness block)") {
		t.Fatalf("doctor under the floor = %+v", got)
	}
	if !strings.Contains(got.Text, "### Policy floor (1)\n- /.well-known/demarkus/policy.md: strictness warn is below the floor's block") {
		t.Errorf("floor finding missing:\n%s", got.Text)
	}
	floorHost = "nowhere"
	if got := tools.Doctor(t.Context(), marktools.DoctorArgs{URL: "/"}); !got.IsError || !strings.Contains(got.Text, "policy-unavailable") {
		t.Errorf("unreadable floor = %+v, want the audit refused", got)
	}
}
