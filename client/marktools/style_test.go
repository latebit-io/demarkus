package marktools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

// The style rules themselves are tested in client/docwrite/gate; here only
// how a publish carries their verdict at each strictness.
func TestPublishStyleGate(t *testing.T) {
	const flawed = "No heading here — and an em dash.\n"
	styled := func(strictness publishpolicy.Strictness, guide string) marktools.Hooks {
		hooks := writingHooks()
		hooks.Style = func(context.Context, marktools.Target) marktools.StyleGate {
			return marktools.StyleGate{Strictness: strictness, Guide: guide}
		}
		return hooks
	}
	publish := func(t *testing.T, hooks marktools.Hooks, body string) (marktools.Result, *fetchtest.Client) {
		t.Helper()
		backend := &fetchtest.Client{}
		got := newTools(t, backend, hooks).Publish(t.Context(), marktools.PublishArgs{
			URL: "/doc.md", Body: body, ExpectedVersion: new(0), OnConflict: "fail", Metadata: map[string]string{"tags": "a"},
		})
		return got, backend
	}

	t.Run("no hook warns and cites the baseline", func(t *testing.T) {
		got, backend := publish(t, writingHooks(), flawed)
		if got.IsError || !strings.HasPrefix(got.Text, "status: created\nversion: 1\n") {
			t.Fatalf("publish = %+v", got)
		}
		for _, want := range []string{"\nnote: style problems in this publish: h1: no `# H1`", "Also: em-dash: 1 em dash", "Rules: metadata out of band"} {
			if !strings.Contains(got.Text, want) {
				t.Errorf("missing %q in:\n%s", want, got.Text)
			}
		}
		if len(backend.PublishCalls) != 1 {
			t.Errorf("publishes = %d, want the write to land", len(backend.PublishCalls))
		}
	})

	t.Run("clean body has no note", func(t *testing.T) {
		got, _ := publish(t, writingHooks(), "# Doc\n\nWhat this settles and for whom.\n")
		if got.IsError || strings.Contains(got.Text, "note:") {
			t.Fatalf("publish = %+v", got)
		}
	})

	t.Run("block refuses before the network", func(t *testing.T) {
		got, backend := publish(t, styled(publishpolicy.Block, "See the guide."), flawed)
		if !got.IsError || !strings.HasPrefix(got.Text, "publish refused, style problems: h1: no `# H1`") || !strings.HasSuffix(got.Text, "See the guide. Fix the body and republish.") {
			t.Fatalf("publish = %+v", got)
		}
		if len(backend.PublishCalls) != 0 {
			t.Error("a refused publish must not reach the backend")
		}
	})

	t.Run("ask lands as warn", func(t *testing.T) {
		got, backend := publish(t, styled(publishpolicy.Ask, ""), flawed)
		if got.IsError || !strings.Contains(got.Text, "\nnote: style problems") || !strings.Contains(got.Text, "Rules: metadata out of band") {
			t.Fatalf("publish = %+v", got)
		}
		if len(backend.PublishCalls) != 1 {
			t.Errorf("publishes = %d, want the write to land", len(backend.PublishCalls))
		}
	})
}
