package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/protocol"
)

// fakeLocal routes the authorities it knows; err fails every exchange.
// A watch is answered with blocks, then held open until it is closed.
type fakeLocal struct {
	served  map[string]protocol.Response
	calls   []protocol.Request
	grants  []protocol.Grant
	err     error
	blocks  []protocol.WatchBlock
	watches chan protocol.Request
	t       *testing.T // reports the watch's pipe errors
}

func (f *fakeLocal) Watch(_ context.Context, authority string, req protocol.Request) (net.Conn, error) {
	if !f.Routes(authority) {
		return nil, errors.New("unrouted authority")
	}
	f.watches <- req
	client, server := net.Pipe()
	go func() {
		for _, block := range f.blocks {
			if _, err := block.WriteTo(server); err != nil {
				return
			}
		}
		// Held open until the client end closes it.
		if _, err := io.Copy(io.Discard, server); err != nil {
			f.t.Errorf("drain watch: %v", err)
		}
		if err := server.Close(); err != nil {
			f.t.Errorf("close watch: %v", err)
		}
	}()
	return client, nil
}

func (f *fakeLocal) Routes(authority string) bool {
	_, ok := f.served[authority]
	return ok
}

// localConfig is the test config with team-a marked local.
func localConfig() *core.Config {
	cfg := brokertest.NewConfig()
	cfg.Worlds[0].Local = true
	return cfg
}

func (f *fakeLocal) Exchange(ctx context.Context, authority string, req protocol.Request) (protocol.Response, error) {
	f.calls = append(f.calls, req)
	if grant, ok := protocol.GrantFrom(ctx); ok {
		f.grants = append(f.grants, grant)
	}
	if f.err != nil {
		return protocol.Response{}, f.err
	}
	return f.served[authority], nil
}

func TestCompositeServesLocalWorldWithWireRequest(t *testing.T) {
	cfg := localConfig()
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

func TestCompositeDispatchesRemoteWorldOverTheNetwork(t *testing.T) {
	cfg := brokertest.NewConfig()
	local := &fakeLocal{served: map[string]protocol.Response{"team-a.team-a.svc.cluster.local": {Status: protocol.StatusOK, Body: "local"}}}
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
	if len(local.calls) != 0 {
		t.Fatal("a world not marked local was served in process")
	}
}

func TestCompositeDoesNotRetryLocalFailureRemotely(t *testing.T) {
	cfg := localConfig()
	boom := errors.New("pipe broke")
	local := &fakeLocal{err: boom}
	remote := &fakeDispatcher{}
	c := NewComposite(cfg.Registry(), local, remote)

	_, err := c.Publish(granted(), fetch.WriteRequest{Host: "team-a", Path: "/a.md", Body: "x"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the local failure", err)
	}
	if len(remote.PublishCalls) != 0 {
		t.Fatal("a failed local write was retried over the network")
	}
}

func TestCompositeRefusesUnknownWorldAndBadRequests(t *testing.T) {
	cfg := localConfig()
	local := &fakeLocal{served: map[string]protocol.Response{"team-a.team-a.svc.cluster.local": {Status: protocol.StatusOK}}}
	c := NewComposite(cfg.Registry(), local, &fakeDispatcher{})

	var notFound *errWorldNotFound
	if _, err := c.Fetch(context.Background(), fetch.FetchRequest{Host: "nowhere", Path: "/"}); !errors.As(err, &notFound) {
		t.Fatalf("unknown world err = %v, want errWorldNotFound", err)
	}
	if _, err := c.Append(granted(), fetch.WriteRequest{Host: "team-a", Path: "/a.md"}); err == nil {
		t.Fatal("append without body or version was dispatched")
	}
	if _, err := c.Lookup(context.Background(), fetch.LookupRequest{Host: "team-a", Scope: "/"}); err == nil {
		t.Fatal("lookup without query was dispatched")
	}
}

// granted is a context the write gate has marked, as toolWriter does.
func granted() context.Context {
	return protocol.WithGrant(context.Background(), protocol.Grant{Label: "alice@example.com", Paths: []string{"/**"}})
}

func TestCompositeWritesLocalWorldUnderTheGrant(t *testing.T) {
	cfg := localConfig()
	local := &fakeLocal{served: map[string]protocol.Response{
		"team-a.team-a.svc.cluster.local": {Status: protocol.StatusCreated},
	}}
	c := NewComposite(cfg.Registry(), local, &fakeDispatcher{})

	res, err := c.Publish(granted(), fetch.WriteRequest{Host: "team-a", Path: "/a.md", Body: "x"})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.Response.Status != protocol.StatusCreated {
		t.Fatalf("status = %q, want created", res.Response.Status)
	}
	if len(local.grants) != 1 || local.grants[0].Label != "alice@example.com" || len(local.grants[0].Paths) != 1 || local.grants[0].Paths[0] != "/**" {
		t.Fatalf("grants reaching the world = %+v, want alice on /**", local.grants)
	}
	if req := local.calls[0]; req.Verb != protocol.VerbPublish || req.Metadata["auth"] != "" {
		t.Fatalf("wire request = %+v, want a tokenless PUBLISH", req)
	}
}

func TestCompositeRefusesWritesWithoutGrantOrLocalWorld(t *testing.T) {
	cfg := brokertest.NewConfig()
	local := &fakeLocal{}
	remote := &fakeDispatcher{}
	c := NewComposite(cfg.Registry(), local, remote)

	if _, err := c.Archive(context.Background(), fetch.ArchiveRequest{Host: "team-a", Path: "/a.md"}); !errors.Is(err, core.ErrNotAuthorized) {
		t.Fatalf("ungranted write err = %v, want ErrNotAuthorized", err)
	}
	if _, err := c.Publish(granted(), fetch.WriteRequest{Host: "team-a", Path: "/a.md", Body: "x"}); !errors.Is(err, errWorldNotLocal) {
		t.Fatalf("remote world write err = %v, want errWorldNotLocal", err)
	}
	if len(local.calls) != 0 || len(remote.PublishCalls) != 0 {
		t.Fatal("a refused write reached a world")
	}
}

func TestCheckLocalRequiresTheServerToRouteEveryLocalWorld(t *testing.T) {
	cfg := localConfig()
	if err := CheckLocal(cfg.Registry(), nil); err == nil || !strings.Contains(err.Error(), "no knowledge server") {
		t.Fatalf("no server err = %v, want a local world refused", err)
	}
	unrouted := &fakeLocal{}
	if err := CheckLocal(cfg.Registry(), unrouted); err == nil || !strings.Contains(err.Error(), "team-a.team-a.svc.cluster.local") {
		t.Fatalf("unrouted err = %v, want the authority named", err)
	}
	routed := &fakeLocal{served: map[string]protocol.Response{"team-a.team-a.svc.cluster.local": {}}}
	if err := CheckLocal(cfg.Registry(), routed); err != nil {
		t.Fatalf("routed: %v", err)
	}
	if err := CheckLocal(brokertest.NewConfig().Registry(), nil); err != nil {
		t.Fatalf("remote only worlds need no server: %v", err)
	}
}

func TestCompositeWatchesLocalWorldInProcess(t *testing.T) {
	cursor := protocol.Cursor{Epoch: "e1", Seq: 4}
	local := &fakeLocal{
		served:  map[string]protocol.Response{"team-a.team-a.svc.cluster.local": {}},
		watches: make(chan protocol.Request, 1),
		t:       t,
		blocks: []protocol.WatchBlock{
			protocol.WatchControl(protocol.StatusOK, cursor),
			protocol.WatchEvent{Cursor: protocol.Cursor{Epoch: "e1", Seq: 5}, Path: "/a.md", Version: 2, Op: protocol.OpPublish}.Block(),
		},
	}
	c := NewComposite(localConfig().Registry(), local, &fakeDispatcher{})
	w, err := c.Watch(context.Background(), fetch.WatchRequest{Host: "team-a", Path: "/", Since: protocol.Cursor{Epoch: "e1", Seq: 3}})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer w.Close()
	if req := <-local.watches; req.Verb != protocol.VerbWatch || req.Path != "/" || req.Metadata["since"] != "e1:3" {
		t.Errorf("wire request = %+v, want WATCH / since e1:3", req)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, err := w.Next(ctx)
	if err != nil || n.Resync || n.Event.Path != "/a.md" || n.Event.Version != 2 {
		t.Fatalf("notice = %+v, %v; want publish /a.md v2", n, err)
	}
}

func TestCompositeWatchRefusesRemoteAndUnknownWorlds(t *testing.T) {
	c := NewComposite(brokertest.NewConfig().Registry(), &fakeLocal{}, &fakeDispatcher{})
	if _, err := c.Watch(context.Background(), fetch.WatchRequest{Host: "team-a", Path: "/"}); !errors.Is(err, errWorldNotLocal) {
		t.Errorf("remote world err = %v, want errWorldNotLocal", err)
	}
	var unknown *errWorldNotFound
	if _, err := c.Watch(context.Background(), fetch.WatchRequest{Host: "nope", Path: "/"}); !errors.As(err, &unknown) {
		t.Errorf("unknown world err = %v, want errWorldNotFound", err)
	}
}
