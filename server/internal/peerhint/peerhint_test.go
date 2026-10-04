package peerhint

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/server/internal/quicserve"
	devtls "github.com/latebit-io/demarkus/server/internal/tls"
	"github.com/quic-go/quic-go"
)

var discardLogger = slog.New(slog.DiscardHandler)

// peer is one in-process replica end: a listener that records hints.
type peer struct {
	addr string
	mu   sync.Mutex
	got  []Hint
	cert *tls.Config
}

func listen(t *testing.T) *peer {
	t.Helper()
	serverTLS, err := devtls.GenerateDevConfig()
	if err != nil {
		t.Fatal(err)
	}
	serverTLS.NextProtos = []string{ALPN}
	server, err := quicserve.Listen(quicserve.Config{Address: "127.0.0.1:0", TLSConfig: serverTLS, Logger: discardLogger})
	if err != nil {
		t.Fatal(err)
	}
	p := &peer{addr: server.Addr().String(), cert: serverTLS}
	endpoint := &Endpoint{Logger: discardLogger, OnHint: func(h Hint) {
		p.mu.Lock()
		p.got = append(p.got, h)
		p.mu.Unlock()
	}}
	served := make(chan error, 1)
	go func() {
		served <- server.Serve(context.Background(), func(*quic.Conn) (quicserve.Endpoint, error) { return endpoint, nil })
	}()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close peer: %v", err)
		}
		if err := <-served; !errors.Is(err, quicserve.ErrServerClosed) {
			t.Errorf("serve peer: %v", err)
		}
	})
	return p
}

func (p *peer) hints() []Hint {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Hint(nil), p.got...)
}

func (p *peer) waitFor(t *testing.T, n int) []Hint {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(p.hints()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("hints = %v, want %d", p.hints(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return p.hints()
}

func ownCertificate(config *tls.Config) func() (*tls.Certificate, error) {
	return func() (*tls.Certificate, error) { return &config.Certificates[0], nil }
}

func runSender(t *testing.T, own *tls.Config, peers ...string) *Sender {
	t.Helper()
	sender := NewSender(SenderConfig{Peers: StaticPeers(peers), TLS: ClientTLS(ownCertificate(own)), Logger: discardLogger})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sender.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return sender
}

// A hint reaches every peer holding the same certificate; a burst
// coalesces to the newest sequence per world.
func TestHintsReachPeersAndCoalesce(t *testing.T) {
	a, b := listen(t), listen(t)
	b.cert = a.cert
	// b serves a's certificate too: the test's stand-in for a shared Secret.
	sender := runSender(t, a.cert, a.addr)
	sender.Hint("world-1", 7)
	if got := a.waitFor(t, 1); got[0] != (Hint{WorldID: "world-1", Sequence: 7}) {
		t.Fatalf("hint = %+v", got[0])
	}
	// Queue a burst before the loop wakes: one hint per world leaves.
	for seq := int64(8); seq <= 200; seq++ {
		sender.Hint("world-1", seq)
	}
	sender.Hint("world-2", 3)
	// Streams carry hints independently, so they may arrive out of order;
	// a hint is a trigger, so only the newest per world matters.
	deadline := time.Now().Add(5 * time.Second)
	for !delivered(a.hints(), Hint{WorldID: "world-1", Sequence: 200}) || !delivered(a.hints(), Hint{WorldID: "world-2", Sequence: 3}) {
		if time.Now().After(deadline) {
			t.Fatalf("hints after the burst = %v", a.hints())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := a.hints(); len(got) > 20 {
		t.Fatalf("%d hints sent for a burst of 193, want a handful", len(got))
	}
}

func delivered(hints []Hint, want Hint) bool { return slices.Contains(hints, want) }

// A peer with another certificate is not a replica: nothing reaches it,
// and the sender keeps going for the peers that are.
func TestSenderRefusesAStrangersCertificate(t *testing.T) {
	stranger := listen(t)
	replica := listen(t)
	sender := runSender(t, replica.cert, stranger.addr, replica.addr)
	sender.Hint("world-1", 1)
	replica.waitFor(t, 1)
	time.Sleep(50 * time.Millisecond)
	if got := stranger.hints(); len(got) != 0 {
		t.Fatalf("the stranger received %v", got)
	}
}

// The endpoint takes one well-formed line and drops the rest.
func TestEndpointParsesOneLine(t *testing.T) {
	var got []Hint
	endpoint := &Endpoint{Logger: discardLogger, OnHint: func(h Hint) { got = append(got, h) }}
	for _, line := range []string{"commit 52b471f7 9\n", "commit 52b471f7 nine\n", "peek 52b471f7 9\n", "commit 52b471f7 0\n", strings.Repeat("x", 300) + "\n", "commit 52b471f7 10"} {
		endpoint.ServeStream(context.Background(), nil, &memStream{Reader: strings.NewReader(line)})
	}
	if len(got) != 1 || got[0] != (Hint{WorldID: "52b471f7", Sequence: 9}) {
		t.Fatalf("hints = %v, want the one well-formed line", got)
	}
}

type memStream struct {
	io.Reader
}

func (memStream) Write(p []byte) (int, error)      { return len(p), nil }
func (memStream) Close() error                     { return nil }
func (memStream) SetReadDeadline(time.Time) error  { return nil }
func (memStream) SetWriteDeadline(time.Time) error { return nil }

// A rollout moves every peer to a new address; the sender must let go of
// connections to the old ones, or its map grows with every rollout.
func TestSenderForgetsDepartedPeers(t *testing.T) {
	a := listen(t)
	var current atomic.Pointer[[]string]
	current.Store(&[]string{a.addr})
	sender := NewSender(SenderConfig{
		Peers:  func(context.Context) ([]string, error) { return *current.Load(), nil },
		TLS:    ClientTLS(ownCertificate(a.cert)),
		Logger: discardLogger,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sender.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	sender.Hint("world-1", 1)
	a.waitFor(t, 1)
	current.Store(&[]string{})
	sender.Hint("world-1", 2)
	deadline := time.Now().Add(5 * time.Second)
	for {
		sender.mu.Lock()
		held := len(sender.conns)
		sender.mu.Unlock()
		if held == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sender still holds %d connections to departed peers", held)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
