// Package peerhint carries commit hints between the replicas of one
// knowledge server: a world committed through a sequence. A hint is a
// trigger, never the change itself: the peer's store reads what was
// committed. A lost hint costs latency until the store's own backstop; a
// forged one costs one read.
//
// It also carries notes: small opaque lines the layer above shares between
// replicas, best effort. A note's taker bounds what a lost or forged one costs.
package peerhint

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/server/internal/quicserve"
	"github.com/quic-go/quic-go"
)

// ALPN is the protocol replicas speak to each other, on a port of its own
// so a network policy can keep it inside the replica set.
const ALPN = "mark-peer"

// One message per stream, one line: "commit <world id> <sequence>" or
// "note <payload>".
const (
	maxLineLength = 1024
	// maxNotes bounds the notes waiting for the sender; past it the oldest go.
	maxNotes = 256
	// noteFlush gathers notes into one batch: every batch resolves the peers.
	noteFlush     = time.Second
	streamTimeout = 5 * time.Second
	dialTimeout   = 3 * time.Second
)

// Hint says a world committed through Sequence.
type Hint struct {
	WorldID  string
	Sequence int64
}

func (h Hint) line() string {
	return "commit " + h.WorldID + " " + strconv.FormatInt(h.Sequence, 10) + "\n"
}

func parseHint(line string) (Hint, error) {
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != "commit" {
		return Hint{}, fmt.Errorf("malformed hint %q", strings.TrimSpace(line))
	}
	if len(fields[1]) > 64 {
		return Hint{}, errors.New("world id too long")
	}
	sequence, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || sequence < 1 {
		return Hint{}, fmt.Errorf("invalid sequence %q", fields[2])
	}
	return Hint{WorldID: fields[1], Sequence: sequence}, nil
}

// noteLine is the line carrying payload, refused when a peer could not read
// it back as one line.
func noteLine(payload string) (string, error) {
	if strings.ContainsAny(payload, "\r\n") {
		return "", errors.New("peerhint: note payload spans lines")
	}
	line := "note " + payload + "\n"
	if len(line) > maxLineLength {
		return "", fmt.Errorf("peerhint: note of %d bytes exceeds %d", len(line), maxLineLength)
	}
	return line, nil
}

// Endpoint serves hints and notes from peers; every stream carries one.
type Endpoint struct {
	OnHint func(Hint)
	// OnNote takes a note; nil drops them. The payload is cut from the
	// stream's line: clone what outlives the call.
	OnNote func(payload string)
	Logger *slog.Logger
}

// ServeStream reads one line and hands it to OnHint or OnNote.
func (e *Endpoint) ServeStream(_ context.Context, remote net.Addr, stream quicserve.Stream) {
	defer func() {
		if err := stream.Close(); err != nil {
			e.Logger.Debug("peerhint: closing stream", "error", err)
		}
	}()
	if err := stream.SetReadDeadline(time.Now().Add(streamTimeout)); err != nil {
		e.Logger.Debug("peerhint: setting read deadline", "error", err)
	}
	line, err := bufio.NewReader(io.LimitReader(stream, maxLineLength)).ReadString('\n')
	if err != nil {
		e.Logger.Warn("peerhint: reading hint", "remote", remote, "error", err)
		return
	}
	if payload, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "note "); ok {
		if e.OnNote != nil {
			e.OnNote(payload)
		}
		return
	}
	hint, err := parseHint(line)
	if err != nil {
		e.Logger.Warn("peerhint: rejected hint", "remote", remote, "error", err)
		return
	}
	e.OnHint(hint)
}

// SenderConfig wires a Sender.
type SenderConfig struct {
	// Peers resolves the addresses to hint on each flush; a failure drops
	// the batch, which the stores' backstop covers.
	Peers  func(ctx context.Context) ([]string, error)
	TLS    *tls.Config
	Logger *slog.Logger
}

// Sender pushes hints and notes to every peer. Hints coalesce per world, so
// a burst of commits costs each peer one hint carrying the newest sequence.
type Sender struct {
	config SenderConfig

	mu      sync.Mutex
	pending map[string]int64
	notes   []string // lines, oldest first
	// flushing is set while a noteFlush timer is due to wake the sender.
	flushing bool
	conns    map[string]*quic.Conn
	wake     chan struct{}
}

// NewSender makes a sender; Run must be started for hints to leave.
func NewSender(config SenderConfig) *Sender {
	return &Sender{config: config, pending: make(map[string]int64), conns: make(map[string]*quic.Conn), wake: make(chan struct{}, 1)}
}

// Hint queues a hint; it never blocks.
func (s *Sender) Hint(worldID string, sequence int64) {
	s.mu.Lock()
	if sequence > s.pending[worldID] {
		s.pending[worldID] = sequence
	}
	s.mu.Unlock()
	s.wakeUp()
}

// Note queues payload for every peer, to leave within noteFlush; it never
// blocks. A full queue drops its oldest note, which a taker must tolerate anyway.
func (s *Sender) Note(payload string) error {
	line, err := noteLine(payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	dropped := len(s.notes) >= maxNotes
	if dropped {
		s.notes = slices.Delete(s.notes, 0, 1)
	}
	s.notes = append(s.notes, line)
	flush := !s.flushing
	s.flushing = true
	s.mu.Unlock()
	if dropped {
		s.config.Logger.Debug("peerhint: note queue full; oldest dropped")
	}
	if flush {
		time.AfterFunc(noteFlush, s.wakeUp)
	}
	return nil
}

func (s *Sender) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run sends queued hints until ctx ends, then closes the peer connections.
func (s *Sender) Run(ctx context.Context) {
	defer s.closeConns()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		batch := s.take()
		if len(batch) == 0 {
			continue
		}
		peers, err := s.config.Peers(ctx)
		if err != nil {
			s.config.Logger.Warn("peerhint: resolving peers; batch dropped", "error", err)
			continue
		}
		s.forgetGone(peers)
		// Each peer on its own: an unreachable one must not hold up the rest.
		var sends sync.WaitGroup
		for _, peer := range peers {
			sends.Go(func() {
				for _, line := range batch {
					if !s.send(ctx, peer, line) {
						break // one timeout per unreachable peer, not one per line
					}
				}
			})
		}
		sends.Wait()
	}
}

// take empties the queues into the lines to send, hints first.
func (s *Sender) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := make([]string, 0, len(s.pending)+len(s.notes))
	for worldID, sequence := range s.pending {
		batch = append(batch, Hint{WorldID: worldID, Sequence: sequence}.line())
	}
	clear(s.pending)
	batch = append(batch, s.notes...)
	s.notes, s.flushing = nil, false
	return batch
}

// send opens one stream for the line and closes it without waiting for an
// answer, reporting whether it went. A failure drops the connection so the
// next line redials.
func (s *Sender) send(ctx context.Context, peer, line string) bool {
	conn, err := s.conn(ctx, peer)
	if err != nil {
		s.config.Logger.Debug("peerhint: dialing peer", "peer", peer, "error", err)
		return false
	}
	openCtx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()
	stream, err := conn.OpenStreamSync(openCtx)
	if err == nil {
		_, err = stream.Write([]byte(line))
		err = errors.Join(err, stream.Close())
	}
	if err != nil {
		s.config.Logger.Debug("peerhint: sending line", "peer", peer, "error", err)
		s.drop(peer, conn)
		return false
	}
	return true
}

func (s *Sender) conn(ctx context.Context, peer string) (*quic.Conn, error) {
	s.mu.Lock()
	conn, ok := s.conns[peer]
	s.mu.Unlock()
	if ok && conn.Context().Err() == nil {
		return conn, nil
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := quic.DialAddr(dialCtx, peer, s.config.TLS, nil)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.conns[peer] = conn
	s.mu.Unlock()
	return conn, nil
}

func (s *Sender) drop(peer string, conn *quic.Conn) {
	s.mu.Lock()
	if s.conns[peer] == conn {
		delete(s.conns, peer)
	}
	s.mu.Unlock()
	if err := conn.CloseWithError(0, "hint failed"); err != nil {
		s.config.Logger.Debug("peerhint: closing peer connection", "peer", peer, "error", err)
	}
}

// forgetGone closes connections to peers no longer resolved. Pods move to
// new addresses on every rollout, so without this the map only grows.
func (s *Sender) forgetGone(peers []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for peer, conn := range s.conns {
		if slices.Contains(peers, peer) {
			continue
		}
		delete(s.conns, peer)
		if err := conn.CloseWithError(0, "peer gone"); err != nil {
			s.config.Logger.Debug("peerhint: closing departed peer connection", "peer", peer, "error", err)
		}
	}
}

func (s *Sender) closeConns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for peer, conn := range s.conns {
		if err := conn.CloseWithError(0, "shutting down"); err != nil {
			s.config.Logger.Debug("peerhint: closing peer connection", "peer", peer, "error", err)
		}
		delete(s.conns, peer)
	}
}

// ClientTLS is the sender's TLS config. Replicas share one certificate, so
// a peer proves itself by presenting exactly this replica's; no CA needed.
// VerifyConnection, unlike VerifyPeerCertificate, also runs on resumed sessions.
func ClientTLS(own func() (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{ALPN},
		InsecureSkipVerify: true, //nolint:gosec // the peer is checked against this replica's own certificate below
		VerifyConnection: func(state tls.ConnectionState) error {
			mine, err := own()
			if err != nil {
				return err
			}
			peer := state.PeerCertificates
			if len(peer) == 0 || len(mine.Certificate) == 0 || !bytes.Equal(peer[0].Raw, mine.Certificate[0]) {
				return errors.New("peerhint: peer does not hold this replica's certificate")
			}
			return nil
		},
	}
}

// StaticPeers hints a fixed address list.
func StaticPeers(addresses []string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { return addresses, nil }
}

// ServicePeers resolves a headless Service's addresses on port, leaving
// out this host's own.
func ServicePeers(service string, port int) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, service)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", service, err)
		}
		local, err := localAddresses()
		if err != nil {
			return nil, err
		}
		peers := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			if local[addr.IP.String()] {
				continue
			}
			peers = append(peers, net.JoinHostPort(addr.IP.String(), strconv.Itoa(port)))
		}
		return peers, nil
	}
}

func localAddresses() (map[string]bool, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}
	local := make(map[string]bool, len(addrs))
	for _, addr := range addrs {
		if ip, ok := addr.(*net.IPNet); ok {
			local[ip.IP.String()] = true
		}
	}
	return local, nil
}
