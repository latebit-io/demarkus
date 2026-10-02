package knowledgeserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
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
	h := newHarness(t.TempDir())
	if err := h.open(t, "worlds:\n"+worldFragment("alice", testWorldID, tokens, false), files); err != nil {
		t.Fatalf("open: %v", err)
	}
	server := &Server{worlds: h.manager}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := server.Exchange(ctx, bearerAuthority, protocol.Request{Verb: protocol.VerbFetch, Path: publishpolicy.DocumentPath})
	if err != nil || resp.Status != protocol.StatusOK {
		t.Fatalf("seeded policy: status %q, err %v, want it served", resp.Status, err)
	}
	publishDoc(t, server, "/notes/a.md")
	if resp, err = server.Exchange(ctx, bearerAuthority, protocol.Request{Verb: protocol.VerbFetch, Path: "/notes/a.md"}); err != nil || resp.Body != "# Note\n" {
		t.Fatalf("fetch: status %q, err %v, body %q", resp.Status, err, resp.Body)
	}
	// A store no replica shares takes no hint; the manager asks nothing else.
	h.manager.Hint(peerhint.Hint{WorldID: testWorldID, Sequence: 99})

	// The world's runtime owns its store and closes it with the world.
	h.manager.Close()
	if _, err := opened.Publish(context.Background(), backend.WriteRequest{Path: "/notes/b.md", ExpectedVersion: -1, Content: []byte("# B\n")}); !errors.Is(err, backend.ErrClosed) {
		t.Fatalf("write after the world closed = %v, want backend.ErrClosed", err)
	}
}
