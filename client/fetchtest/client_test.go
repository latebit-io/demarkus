package fetchtest

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
)

func TestPublishedDocumentKeepsPublisherMetadata(t *testing.T) {
	c := &Client{}
	meta := map[string]string{"tags": "a,b", "type": "Note", "version": "99"}
	if _, err := c.Publish("h", "/doc.md", "# Doc\n", "token", 0, meta); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got, err := c.Fetch("h", "/doc.md", "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := map[string]string{"tags": "a,b", "type": "Note", "version": "1"}
	for key, value := range want {
		if got.Response.Metadata[key] != value {
			t.Errorf("metadata[%q] = %q, want %q", key, got.Response.Metadata[key], value)
		}
	}
	if got.Response.Metadata["content-hash"] == "" {
		t.Error("stored document has no content-hash")
	}
	meta["tags"] = "changed"
	again, err := c.Fetch("h", "/doc.md", "")
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if again.Response.Metadata["tags"] != "a,b" {
		t.Error("stored metadata aliases the caller's map")
	}
}

func TestContextMethodsRefuseDoneContextBeforeCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := func(name string) { t.Errorf("%s ran for a context that was already done", name) }
	c := &Client{
		FetchCtxFn: func(context.Context, string, string, string) (fetch.Result, error) {
			called("FetchCtxFn")
			return fetch.Result{}, nil
		},
		LookupCtxFn: func(context.Context, string, string, string, string, fetch.LookupOptions) (fetch.Result, error) {
			called("LookupCtxFn")
			return fetch.Result{}, nil
		},
		LookupFn: func(string, string, string, string, fetch.LookupOptions) (fetch.Result, error) {
			called("LookupFn")
			return fetch.Result{}, nil
		},
	}
	if _, err := c.FetchContext(ctx, "h", "/doc.md", ""); !errors.Is(err, context.Canceled) {
		t.Errorf("FetchContext err = %v, want context.Canceled", err)
	}
	if _, err := c.LookupContext(ctx, "h", "/", "q", "", fetch.LookupOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("LookupContext err = %v, want context.Canceled", err)
	}
}
