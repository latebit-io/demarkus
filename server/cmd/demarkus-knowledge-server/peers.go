package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/server/internal/certsource"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
	"github.com/latebit-io/demarkus/server/internal/peerhint"
	"github.com/latebit-io/demarkus/server/internal/quicserve"
	"github.com/quic-go/quic-go"
)

// peerLinks is the hint path between replicas: a sender for this replica's
// commits and a listener for the peers'. Without a peers section both are
// nil and the backstop poll is the only path.
type peerLinks struct {
	sender *peerhint.Sender
	server *quicserve.Server
	stop   context.CancelFunc
	group  sync.WaitGroup

	mu     sync.Mutex
	onHint func(peerhint.Hint)
}

// openPeers starts the sender and the listener config.Peers names.
func openPeers(config knowledgeconfig.PeersConfig, certificates *certsource.Source, logger *slog.Logger) (*peerLinks, error) {
	links := &peerLinks{stop: func() {}}
	if !config.Enabled() {
		return links, nil
	}
	port, err := config.Port()
	if err != nil {
		return nil, err
	}
	server, err := quicserve.Listen(quicserve.Config{
		Address:    config.Listen,
		TLSConfig:  certificates.TLSConfig(peerhint.ALPN),
		QUICConfig: &quic.Config{MaxIncomingStreams: 64, MaxIncomingUniStreams: 0, MaxIdleTimeout: 30 * time.Second},
		Logger:     logger,
	})
	if err != nil {
		return nil, err
	}
	links.server = server
	links.sender = peerhint.NewSender(peerhint.SenderConfig{
		Peers:  resolvePeers(config, port),
		TLS:    peerhint.ClientTLS(func() (*tls.Certificate, error) { return certificates.GetCertificate(nil) }),
		Logger: logger,
	})
	ctx, cancel := context.WithCancel(context.Background())
	links.stop = cancel
	links.group.Go(func() { links.sender.Run(ctx) })
	endpoint := &peerhint.Endpoint{Logger: logger, OnHint: links.dispatch}
	links.group.Go(func() {
		err := server.Serve(ctx, func(*quic.Conn) (quicserve.Endpoint, error) { return endpoint, nil })
		if err != nil && !errors.Is(err, quicserve.ErrServerClosed) {
			logger.Warn("peer listener stopped", "error", err)
		}
	})
	logger.Info("peer hints enabled", "listen", server.Addr(), "service", config.Service, "addresses", len(config.Addresses))
	return links, nil
}

// resolvePeers is the configured Service's members plus the fixed
// addresses.
func resolvePeers(config knowledgeconfig.PeersConfig, port int) func(context.Context) ([]string, error) {
	if config.Service == "" {
		return peerhint.StaticPeers(config.Addresses)
	}
	service := peerhint.ServicePeers(config.Service, port)
	return func(ctx context.Context) ([]string, error) {
		members, err := service(ctx)
		if err != nil {
			return nil, err
		}
		return append(members, config.Addresses...), nil
	}
}

// hint tells the peers a world's head moved; nothing without peers.
func (l *peerLinks) hint(worldID string, sequence int64) {
	if l.sender != nil {
		l.sender.Hint(worldID, sequence)
	}
}

// deliverTo names who takes the peers' hints; hints before that are
// dropped, which the backstop poll covers.
func (l *peerLinks) deliverTo(onHint func(peerhint.Hint)) {
	l.mu.Lock()
	l.onHint = onHint
	l.mu.Unlock()
}

func (l *peerLinks) dispatch(hint peerhint.Hint) {
	l.mu.Lock()
	onHint := l.onHint
	l.mu.Unlock()
	if onHint != nil {
		onHint(hint)
	}
}

// close stops the sender and the listener and waits for both.
func (l *peerLinks) close(logger *slog.Logger) {
	l.stop()
	if l.server != nil {
		if err := l.server.Close(); err != nil {
			logger.Warn("peer listener close failed", "error", err)
		}
	}
	l.group.Wait()
}
