package marktools_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
)

// mvWriter is the stdio server with -host example.com and a token.
func mvWriter(token string) *clientSurface {
	return &clientSurface{DefaultHost: "mark://example.com", Token: token}
}

// mvConflictBackend is a document at v6 whose v5 the agent edited: every
// publish conflicts, and theirs is the v6 body.
func mvConflictBackend(theirs string, publishCalls *int) *fetchtest.Client {
	return &fetchtest.Client{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			switch r.Path {
			case "/doc.md/v5":
				return fetch.Result{Response: protocol.Response{
					Status:   protocol.StatusOK,
					Metadata: map[string]string{"version": "5"},
					Body:     "a\nb\nc\n",
				}}, nil
			case "/doc.md":
				return fetch.Result{Response: protocol.Response{
					Status:   protocol.StatusOK,
					Metadata: map[string]string{"version": "6"},
					Body:     theirs,
				}}, nil
			}
			return fetch.Result{}, nil
		},
		PublishFn: func(_ context.Context, _ fetch.WriteRequest) (fetch.Result, error) {
			if publishCalls != nil {
				*publishCalls++
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusConflict,
				Metadata: map[string]string{"server-version": "6"},
			}}, nil
		},
	}
}

// Latest is v6 ("a\nXX\nc\n"), overlapping the agent's edit: diff3 marks it.
func TestPublishOnConflictMergeOverlap(t *testing.T) {
	tools := mvWriter("test").tools(t, mvConflictBackend("a\nXX\nc\n", nil))
	got := tools.Publish(t.Context(), marktools.PublishArgs{
		URL: "mark://example.com/doc.md", Body: "a\nB\nc\n", ExpectedVersion: new(5), OnConflict: "merge",
	})
	if got.IsError {
		t.Fatalf("unexpected tool error: %v", got.Text)
	}
	for _, want := range []string{
		"status: merge-candidate",
		"has-markers: true",
		"your-version: 5",
		"current-version: 6",
		"publish-at-version: 6",
		"<<<<<<< ours",
		">>>>>>> theirs",
		"B",
		"XX",
	} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("response missing %q\n%s", want, got.Text)
		}
	}
}

// A negative expected_version is refused the same way on the merge path and on
// the fail path, which would otherwise forward it to the server.
func TestPublishNegativeExpectedVersionRejected(t *testing.T) {
	for _, onConflict := range []string{"", "merge", "fail"} {
		t.Run("on_conflict="+onConflict, func(t *testing.T) {
			tools := mvWriter("test").tools(t, &fetchtest.Client{})
			got := tools.Publish(t.Context(), marktools.PublishArgs{
				URL: "mark://example.com/doc.md", Body: "x", ExpectedVersion: new(-1), OnConflict: onConflict,
			})
			mvAssertError(t, got, "expected_version must be >= 0")
		})
	}
}

// MCP clients commonly send blank strings for optional fields: a blank
// on_conflict behaves like omission (merge), not like an invalid value.
func TestPublishBlankOnConflictUsesDefault(t *testing.T) {
	for _, blank := range []string{"", " ", "\t"} {
		t.Run(fmt.Sprintf("on_conflict=%q", blank), func(t *testing.T) {
			backend := &fetchtest.Client{
				// ok, so the merge branch short-circuits without fetch fixtures.
				PublishFn: func(_ context.Context, _ fetch.WriteRequest) (fetch.Result, error) {
					return fetch.Result{Response: protocol.Response{
						Status:   protocol.StatusCreated,
						Metadata: map[string]string{"version": "1"},
					}}, nil
				},
			}
			tools := mvWriter("test").tools(t, backend)
			got := tools.Publish(t.Context(), marktools.PublishArgs{
				URL: "mark://example.com/doc.md", Body: "x", ExpectedVersion: new(0), OnConflict: blank,
			})
			if got.IsError {
				t.Fatalf("blank on_conflict should not error; got: %v", got.Text)
			}
		})
	}
}

// on_conflict "fail" keeps strict optimistic concurrency: the server's
// conflict reaches the agent verbatim, no diff3.
func TestPublishOnConflictFailOptOut(t *testing.T) {
	var fetchCalls int
	backend := &fetchtest.Client{
		FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
			fetchCalls++
			return fetch.Result{}, nil
		},
		PublishFn: func(_ context.Context, _ fetch.WriteRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusConflict,
				Metadata: map[string]string{"server-version": "6", "your-version": "5"},
			}}, nil
		},
	}
	tools := mvWriter("test").tools(t, backend)
	got := tools.Publish(t.Context(), marktools.PublishArgs{
		URL: "mark://example.com/doc.md", Body: "x", ExpectedVersion: new(5), OnConflict: "fail",
	})
	if got.IsError {
		t.Fatalf("unexpected tool error: %v", got.Text)
	}
	if fetchCalls != 0 {
		t.Errorf("fail mode must not fetch base/current; saw %d fetches", fetchCalls)
	}
	if !strings.Contains(got.Text, "status: conflict") {
		t.Errorf("fail mode should surface raw conflict status, got:\n%s", got.Text)
	}
	if strings.Contains(got.Text, "merge-candidate") {
		t.Errorf("fail mode must not produce a merge candidate, got:\n%s", got.Text)
	}
}

// mvPublishedAtV5 is a document at v5 whose next publish the server accepts.
func mvPublishedAtV5(accepted map[string]string) *fetchtest.Client {
	return &fetchtest.Client{
		Published: map[string]fetch.Result{
			"example.com:6309/doc.md": {Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "5"}}},
		},
		PublishFn: func(_ context.Context, _ fetch.WriteRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusCreated, Metadata: accepted}}, nil
		},
	}
}

// on_conflict=merge must not change the success contract: every metadata key
// the server returned reaches the agent, as a plain mark_publish would show.
func TestPublishOnConflictMergeFirstTrySuccessPreservesAllMetadata(t *testing.T) {
	backend := mvPublishedAtV5(map[string]string{
		"version":      "6",
		"modified":     "2026-05-05T00:00:00Z",
		"content-hash": "sha256-deadbeef",
		"etag":         "abc123",
		"custom-key":   "preserved",
	})
	tools := mvWriter("test").tools(t, backend)
	got := tools.Publish(t.Context(), marktools.PublishArgs{
		URL: "mark://example.com/doc.md", Body: "x", ExpectedVersion: new(5), OnConflict: "merge",
	})
	if got.IsError {
		t.Fatalf("unexpected tool error: %v", got.Text)
	}
	for _, want := range []string{
		"status: created",
		"version: 6",
		"modified: 2026-05-05T00:00:00Z",
		"content-hash: sha256-deadbeef",
		"etag: abc123",
		"custom-key: preserved",
	} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("response missing %q\n%s", want, got.Text)
		}
	}
}
