package knowledgeserver

import (
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/server/internal/certsource"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
)

func openTestPeers(t *testing.T, certs *certsource.Source, addresses ...string) *peerLinks {
	t.Helper()
	links, err := openPeers(knowledgeconfig.PeersConfig{Listen: "127.0.0.1:0", Addresses: addresses}, certs, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { links.close(slog.New(slog.DiscardHandler)) })
	return links
}

// A note one replica sends reaches the taker another named, and no one once
// the taker is gone.
func TestPeerNotesReachTheTaker(t *testing.T) {
	certFile, keyFile := testCertFiles(t, "peers.test")
	certs, err := certsource.Open(certFile, keyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	receiver := openTestPeers(t, certs)
	sender := openTestPeers(t, certs, receiver.server.Addr().String())

	var mu sync.Mutex
	var got []string
	receiver.takeNotes(func(payload string) {
		mu.Lock()
		got = append(got, payload)
		mu.Unlock()
	})
	taken := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
	if err := sender.note("first"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the note to reach its taker", func() bool { return slices.Equal(taken(), []string{"first"}) })

	receiver.takeNotes(nil)
	receiver.dispatchNote("after")
	if got := taken(); len(got) != 1 {
		t.Errorf("taken = %q after the taker left", got)
	}
}

// A server without peers sends nothing and reports no error: one replica
// has no one to tell.
func TestNoteWithoutPeersIsNotAnError(t *testing.T) {
	if err := (&peerLinks{}).note("x"); err != nil {
		t.Fatal(err)
	}
}
