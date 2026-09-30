package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/protocol"
)

// fakeLocal routes the authorities it knows; err fails every exchange.
type fakeLocal struct {
	served map[string]protocol.Response
	calls  []protocol.Request
	err    error
}

func (f *fakeLocal) Routes(authority string) bool {
	_, ok := f.served[authority]
	return ok || f.err != nil
}

func (f *fakeLocal) Exchange(_ context.Context, authority string, req protocol.Request) (protocol.Response, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return protocol.Response{}, f.err
	}
	return f.served[authority], nil
}

func TestCompositeServesLocalWorldWithWireRequest(t *testing.T) {
	cfg := brokertest.NewConfig()
	local := &fakeLocal{served: map[string]protocol.Response{
		"team-a.team-a.svc.cluster.local": {Status: protocol.StatusOK, Body: "local"},
	}}
	remote := &fakeDispatcher{}
	c := NewComposite(cfg.Registry(), local, remote)

	res, err := c.Fetch(context.Background(), fetch.FetchRequest{Host: "team-a", Path: "/index.md", Token: "tok"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.Response.Body != "local" {
		t.Fatalf("body = %q, want local", res.Response.Body)
	}
	if got := len(local.calls); got != 1 {
		t.Fatalf("local calls = %d, want 1", got)
	}
	req := local.calls[0]
	if req.Verb != protocol.VerbFetch || req.Path != "/index.md" || req.Metadata["auth"] != "tok" {
		t.Fatalf("wire request = %+v, want FETCH /index.md with auth", req)
	}
	if remote.FetchCallCount() != 0 {
		t.Fatal("remote dispatcher was used for a local world")
	}
}

func TestCompositeFallsBackToRemoteForUnroutedAuthority(t *testing.T) {
	cfg := brokertest.NewConfig()
	local := &fakeLocal{}
	remote := &fakeDispatcher{ListFn: func(context.Context, fetch.ListRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "remote"}}, nil
	}}
	c := NewComposite(cfg.Registry(), local, remote)

	res, err := c.List(context.Background(), fetch.ListRequest{Host: "team-a", Path: "/"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.Response.Body != "remote" {
		t.Fatalf("body = %q, want remote", res.Response.Body)
	}
}

func TestCompositeDoesNotRetryLocalFailureRemotely(t *testing.T) {
	cfg := brokertest.NewConfig()
	boom := errors.New("pipe broke")
	local := &fakeLocal{err: boom}
	remote := &fakeDispatcher{}
	c := NewComposite(cfg.Registry(), local, remote)

	_, err := c.Publish(context.Background(), fetch.WriteRequest{Host: "team-a", Path: "/a.md", Body: "x"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the local failure", err)
	}
	if len(remote.PublishCalls) != 0 {
		t.Fatal("a failed local write was retried over the network")
	}
}

func TestCompositeRefusesUnknownWorldAndBadRequests(t *testing.T) {
	cfg := brokertest.NewConfig()
	local := &fakeLocal{served: map[string]protocol.Response{"team-a.team-a.svc.cluster.local": {Status: protocol.StatusOK}}}
	c := NewComposite(cfg.Registry(), local, &fakeDispatcher{})

	var notFound *errWorldNotFound
	if _, err := c.Fetch(context.Background(), fetch.FetchRequest{Host: "nowhere", Path: "/"}); !errors.As(err, &notFound) {
		t.Fatalf("unknown world err = %v, want errWorldNotFound", err)
	}
	if _, err := c.Append(context.Background(), fetch.WriteRequest{Host: "team-a", Path: "/a.md"}); err == nil {
		t.Fatal("append without body or version was dispatched")
	}
	if _, err := c.Lookup(context.Background(), fetch.LookupRequest{Host: "team-a", Scope: "/"}); err == nil {
		t.Fatal("lookup without query was dispatched")
	}
}
