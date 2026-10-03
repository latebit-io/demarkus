// Package peerhint carries commit hints between the replicas of one
// knowledge server: a world committed through a sequence. A hint is a
// trigger, never the change itself: the peer's store reads what was
// committed. A lost hint costs latency until the store's own backstop; a
// forged one costs one read.
package peerhint

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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

// One hint per stream, one line: "commit <world id> <sequence>".
const (
	maxLineLength = 256
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

// Endpoint serves hints from peers; every stream carries one.
type Endpoint struct {
	OnHint func(Hint)
	Logger *slog.Logger
}

// ServeStream reads one hint and hands it to OnHint.
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

// Sender pushes hints to every peer. Hints coalesce per world, so a burst
// of commits costs each peer one hint carrying the newest sequence.
type Sender struct {
	config SenderConfig

	mu      sync.Mutex
	pending map[string]int64
	conns   map[string]*quic.Conn
	wake    chan struct{}
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
			s.config.Logger.Warn("peerhint: resolving peers; hints dropped", "error", err)
			continue
		}
		s.forgetGone(peers)
		for _, peer := range peers {
			for _, hint := range batch {
				if !s.send(ctx, peer, hint) {
					break // one timeout per unreachable peer, not one per hint
				}
			}
		}
	}
}

func (s *Sender) take() []Hint {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := make([]Hint, 0, len(s.pending))
	for worldID, sequence := range s.pending {
		batch = append(batch, Hint{WorldID: worldID, Sequence: sequence})
	}
	clear(s.pending)
	return batch
}

// send opens one stream for the hint and closes it without waiting for an
// answer, reporting whether it went. A failure drops the connection so the
// next hint redials.
func (s *Sender) send(ctx context.Context, peer string, hint Hint) bool {
	conn, err := s.conn(ctx, peer)
	if err != nil {
		s.config.Logger.Debug("peerhint: dialing peer", "peer", peer, "error", err)
		return false
	}
	openCtx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()
	stream, err := conn.OpenStreamSync(openCtx)
	if err == nil {
		_, err = stream.Write([]byte(hint.line()))
		err = errors.Join(err, stream.Close())
	}
	if err != nil {
		s.config.Logger.Debug("peerhint: sending hint", "peer", peer, "error", err)
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
func ClientTLS(own func() (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{ALPN},
		InsecureSkipVerify: true, //nolint:gosec // the peer is checked against this replica's own certificate below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			mine, err := own()
			if err != nil {
				return err
			}
			if len(raw) == 0 || len(mine.Certificate) == 0 || !bytes.Equal(raw[0], mine.Certificate[0]) {
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
