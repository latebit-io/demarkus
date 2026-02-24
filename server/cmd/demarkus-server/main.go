package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/latebit/demarkus/protocol"
	"github.com/latebit/demarkus/server/internal/auth"
	"github.com/latebit/demarkus/server/internal/config"
	"github.com/latebit/demarkus/server/internal/handler"
	"github.com/latebit/demarkus/server/internal/logging"
	"github.com/latebit/demarkus/server/internal/ratelimit"
	"github.com/latebit/demarkus/server/internal/store"
	servertls "github.com/latebit/demarkus/server/internal/tls"
	"github.com/quic-go/quic-go"
)

func main() {
	root := flag.String("root", "", "content directory to serve (overrides DEMARKUS_ROOT)")
	port := flag.Int("port", 0, "port to listen on (overrides DEMARKUS_PORT)")
	tlsCert := flag.String("tls-cert", "", "path to TLS certificate PEM file (overrides DEMARKUS_TLS_CERT)")
	tlsKey := flag.String("tls-key", "", "path to TLS private key PEM file (overrides DEMARKUS_TLS_KEY)")
	tokens := flag.String("tokens", "", "path to TOML tokens file for auth (overrides DEMARKUS_TOKENS)")
	flag.Parse()

	cfg, err := config.NewConfig()

	// Create logger early so all subsequent output is structured.
	logger := logging.New(cfg.LogFormat, cfg.LogLevel, nil)

	if err != nil {
		logger.Warn("config", "error", err)
	}

	// Flag overrides take precedence over env vars
	if *root != "" {
		cfg.ContentDir = *root
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if *tlsCert != "" {
		cfg.TLSCert = *tlsCert
	}
	if *tlsKey != "" {
		cfg.TLSKey = *tlsKey
	}
	if *tokens != "" {
		cfg.TokensFile = *tokens
	}
	if cfg.ContentDir == "" {
		logger.Error("content directory is required (set DEMARKUS_ROOT or use -root flag)")
		os.Exit(1)
	}
	info, err := os.Stat(cfg.ContentDir)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Error("content directory does not exist", "path", cfg.ContentDir)
			os.Exit(1)
		}
		logger.Error("cannot stat content directory", "path", cfg.ContentDir, "error", err)
		os.Exit(1)
	} else if !info.IsDir() {
		logger.Error("content directory is not a directory", "path", cfg.ContentDir)
		os.Exit(1)
	}

	tlsConfig, prodMode, err := loadTLS(cfg, logger)
	if err != nil {
		logger.Error("tls setup failed", "error", err)
		os.Exit(1)
	}

	quicConfig := &quic.Config{
		MaxIncomingStreams:    int64(cfg.MaxStreams),
		MaxIncomingUniStreams: 0,
		MaxIdleTimeout:        cfg.IdleTimeout,
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	listener, err := quic.ListenAddr(addr, tlsConfig, quicConfig)
	if err != nil {
		logger.Error("listen failed", "addr", addr, "error", err)
		os.Exit(1)
	}
	defer func() { _ = listener.Close() }()

	s := store.New(cfg.ContentDir)

	if cfg.TokensFile != "" {
		if err := loadTokenStore(cfg.TokensFile); err != nil {
			logger.Error("token loading failed", "error", err)
			os.Exit(1)
		}
		logger.Info("auth: loaded tokens", "path", cfg.TokensFile)
	} else {
		logger.Info("auth: no tokens file configured, writes disabled")
	}

	h := &handler.Handler{
		ContentDir: cfg.ContentDir,
		Store:      s,
		Logger:     logger,
		GetTokenStore: func() *auth.TokenStore {
			tokenMu.RLock()
			defer tokenMu.RUnlock()
			return currentTokenStore
		},
	}

	var rl *ratelimit.Limiter
	if cfg.RateLimit > 0 {
		rl = ratelimit.New(cfg.RateLimit, cfg.RateBurst)
		defer rl.Stop()
		logger.Info("rate limit configured", "req_per_sec", cfg.RateLimit, "burst", cfg.RateBurst)
	}

	logger.Info("server started",
		"addr", addr,
		"root", cfg.ContentDir,
		"idle_timeout", cfg.IdleTimeout.String(),
		"request_timeout", cfg.RequestTimeout.String())

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start SIGHUP handler for certificate reload (Unix only, no-op on Windows)
	startCertReloader(cfg, prodMode, logger)

	// Accept connections in a goroutine so we can listen for shutdown signals
	var wg sync.WaitGroup
	errChan := make(chan error, 1)
	go func() {
		for {
			conn, err := listener.Accept(context.Background())
			if err != nil {
				errChan <- err
				return
			}
			wg.Go(func() {
				handleConn(conn, h, cfg.RequestTimeout, rl, logger)
			})
		}
	}()

	// Wait for shutdown signal or listener error
	select {
	case sig := <-sigChan:
		logger.Info("received signal, initiating graceful shutdown", "signal", sig.String())
	case err := <-errChan:
		logger.Error("listener error", "error", err)
	}

	// Close the listener to stop accepting new connections
	_ = listener.Close()

	// Wait for in-flight connections to drain with a timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Info("all connections drained")
	case <-time.After(10 * time.Second):
		logger.Warn("shutdown timeout: some connections did not finish")
	}

	logger.Info("server stopped")
}

func handleConn(conn *quic.Conn, h *handler.Handler, requestTimeout time.Duration, rl *ratelimit.Limiter, logger *slog.Logger) {
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return // connection closed
		}
		if rl != nil {
			ip := ratelimit.ExtractIP(conn.RemoteAddr())
			if !rl.Allow(ip) {
				logger.Warn("rate limited")
				_ = stream.Close()
				continue
			}
		}
		if requestTimeout > 0 {
			_ = stream.SetReadDeadline(time.Now().Add(requestTimeout))
		}
		go h.HandleStream(stream)
	}
}

var (
	certMu      sync.RWMutex
	currentCert *tls.Certificate

	tokenMu           sync.RWMutex
	currentTokenStore *auth.TokenStore
)

// loadCert loads a TLS certificate from disk and stores it for serving.
func loadCert(certFile, keyFile string) error {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("loading TLS certificate: %w", err)
	}
	certMu.Lock()
	currentCert = &cert
	certMu.Unlock()
	return nil
}

// loadTokenStore reads the tokens file and atomically replaces the current store.
func loadTokenStore(path string) error {
	ts, err := auth.LoadTokens(path)
	if err != nil {
		return err
	}
	tokenMu.Lock()
	currentTokenStore = ts
	tokenMu.Unlock()
	return nil
}

// loadTLS returns a TLS config based on the server configuration.
// In production mode (cert+key provided), uses GetCertificate callback
// so certificates can be reloaded at runtime via SIGHUP.
// If neither is set, generates a self-signed dev certificate.
// Returns an error if only one of cert/key is provided.
func loadTLS(cfg *config.Config, logger *slog.Logger) (tlsConfig *tls.Config, prodMode bool, err error) {
	haveCert := cfg.TLSCert != ""
	haveKey := cfg.TLSKey != ""

	switch {
	case haveCert && haveKey:
		logger.Info("tls: loading certificate", "path", cfg.TLSCert)
		if err := loadCert(cfg.TLSCert, cfg.TLSKey); err != nil {
			return nil, false, err
		}
		return &tls.Config{
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				certMu.RLock()
				defer certMu.RUnlock()
				if currentCert == nil {
					return nil, fmt.Errorf("tls: no certificate loaded")
				}
				return currentCert, nil
			},
			MinVersion: tls.VersionTLS13,
			NextProtos: []string{protocol.ALPN},
		}, true, nil
	case haveCert != haveKey:
		return nil, false, fmt.Errorf("both -tls-cert and -tls-key must be provided (got cert=%q, key=%q)", cfg.TLSCert, cfg.TLSKey)
	default:
		logger.Info("tls: using self-signed dev certificate (set DEMARKUS_TLS_CERT and DEMARKUS_TLS_KEY for production)")
		tc, err := servertls.GenerateDevConfig()
		return tc, false, err
	}
}
