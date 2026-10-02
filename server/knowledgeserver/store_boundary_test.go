package knowledgeserver

import (
	"context"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/filestore"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
	"github.com/latebit-io/demarkus/server/internal/peerhint"
)

// The manager serves a world on whatever store its factory opens; a file
// store knows nothing of buckets, peers or genesis, and still serves.
func TestWorldManagerServesANonBucketStore(t *testing.T) {
	tokens := writeTokens(t, t.TempDir(), "alice")
	var opened *filestore.Store
	files := func(_ context.Context, _ *knowledgeconfig.WorldConfig, hooks storeHooks) (worldStore, error) {
		store, err := filestore.Open(protocolstore.New(t.TempDir()), catalog.New(), filestore.Options{ChangeRing: changefeed.DefaultRingSize, Logger: hooks.logger})
		if err != nil {
			return nil, err
		}
		opened = store
		return store, nil
	}
	h := &worldsTestHarness{dir: t.TempDir(), stores: map[string]*blob.Memory{}}
	if err := h.open(t, "worlds:\n"+worldFragment("alice", testWorldID, tokens, false), files); err != nil {
		t.Fatalf("open: %v", err)
	}
	server := &Server{worlds: h.manager}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const authority = "alice.memory.svc.cluster.local"

	resp, err := server.Exchange(ctx, authority, protocol.Request{Verb: protocol.VerbFetch, Path: publishpolicy.DocumentPath})
	if err != nil || resp.Status != protocol.StatusOK {
		t.Fatalf("seeded policy: status %q, err %v, want it served", resp.Status, err)
	}
	granted := protocol.WithGrant(ctx, protocol.Grant{Label: "alice@example.com", Paths: []string{"/notes/**"}})
	publish := protocol.Request{Verb: protocol.VerbPublish, Path: "/notes/a.md", Body: "# A\n", Metadata: map[string]string{"expected-version": "0"}}
	if resp, err = server.Exchange(granted, authority, publish); err != nil || resp.Status != protocol.StatusCreated {
		t.Fatalf("publish: status %q, err %v (body %q)", resp.Status, err, resp.Body)
	}
	if resp, err = server.Exchange(ctx, authority, protocol.Request{Verb: protocol.VerbFetch, Path: "/notes/a.md"}); err != nil || resp.Body != "# A\n" {
		t.Fatalf("fetch: status %q, err %v, body %q", resp.Status, err, resp.Body)
	}
	// A store no replica shares takes no hint; the manager asks nothing else.
	h.manager.Hint(peerhint.Hint{WorldID: testWorldID, Sequence: 99})

	// The world's runtime owns its store and closes it with the world.
	h.manager.Close()
	if _, err := opened.Publish(context.Background(), backend.WriteRequest{Path: "/notes/b.md", ExpectedVersion: -1, Content: []byte("# B\n")}); err == nil {
		t.Fatal("the store still takes writes after its world closed")
	}
}
