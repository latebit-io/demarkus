package graphstore

import (
	"context"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

type recordingFetcher struct{ requests []fetch.FetchRequest }

func (r *recordingFetcher) Fetch(_ context.Context, req fetch.FetchRequest) (fetch.Result, error) {
	r.requests = append(r.requests, req)
	return fetch.Result{Response: protocol.Response{
		Status: protocol.StatusOK, Body: "# Doc\n", Metadata: map[string]string{"etag": "e1"},
	}}, nil
}

// A crawl follows links to hosts the user never named, so the token is asked
// for per dial host, and any resolver will do: tools bring their own.
func TestNewFetchFuncResolvesATokenPerHost(t *testing.T) {
	client := &recordingFetcher{}
	resolver := fetch.TokenResolverFunc(func(host string) string { return "token-for-" + host })
	fetchFn := NewFetchFunc(client, resolver)

	for _, raw := range []string{"mark://Origin.Example/a.md", "mark://foreign.example:7000/b.md"} {
		target, err := links.ParseMark(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, err := fetchFn(t.Context(), target)
		if err != nil || got.Status != protocol.StatusOK || got.Body != "# Doc\n" || got.Metadata["etag"] != "e1" {
			t.Fatalf("fetch %s = %+v, %v", raw, got, err)
		}
	}
	want := []fetch.FetchRequest{
		{Host: "origin.example:6309", Path: "/a.md", Token: "token-for-origin.example:6309"},
		{Host: "foreign.example:7000", Path: "/b.md", Token: "token-for-foreign.example:7000"},
	}
	if len(client.requests) != len(want) || client.requests[0] != want[0] || client.requests[1] != want[1] {
		t.Errorf("requests = %+v, want %+v", client.requests, want)
	}
}
