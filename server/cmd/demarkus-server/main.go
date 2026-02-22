package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/latebit/demarkus/protocol"
	"github.com/latebit/demarkus/server/internal/auth"
	"github.com/latebit/demarkus/server/internal/config"
	"github.com/latebit/demarkus/server/internal/handler"
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
	if err != nil {
		log.Printf("[WARN] config: %v", err)
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
		log.Fatal("[ERROR] content directory is required (set DEMARKUS_ROOT or use -root flag)")
	}
	info, err := os.Stat(cfg.ContentDir)
	if err != nil {
		if os.IsNotExist(err) {
			log.Fatalf("[ERROR] content directory %q does not exist", cfg.ContentDir)
		}
		log.Fatalf("[ERROR] cannot stat content directory %q: %v", cfg.ContentDir, err)
	} else if !info.IsDir() {
		log.Fatalf("[ERROR] content directory %q is not a directory", cfg.ContentDir)
	}

	tlsConfig, prodMode, err := loadTLS(cfg)
	if err != nil {
		log.Fatalf("[ERROR] %v", err)
	}

	quicConfig := &quic.Config{
		MaxIncomingStreams:    int64(cfg.MaxStreams),
		MaxIncomingUniStreams: 0,
		MaxIdleTimeout:        cfg.IdleTimeout,
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	listener, err := quic.ListenAddr(addr, tlsConfig, quicConfig)
	if err != nil {
		log.Fatalf("[ERROR] listen on %s: %v", addr, err)
	}
	defer func() { _ = listener.Close() }()

	s := store.New(cfg.ContentDir)

	var tokenStore *auth.TokenStore
	if cfg.TokensFile != "" {
		ts, err := auth.LoadTokens(cfg.TokensFile)
		if err != nil {
			log.Fatalf("[ERROR] %v", err)
		}
		tokenStore = ts
		log.Printf("[INFO] auth: loaded tokens from %s", cfg.TokensFile)
	} else {
		log.Printf("[INFO] auth: no tokens file configured, writes disabled")
	}

	h := &handler.Handler{ContentDir: cfg.ContentDir, Store: s, TokenStore: tokenStore}

	log.Printf("[INFO] demarkus-server listening on %s (root: %s, idle_timeout: %v, request_timeout: %v)",
		addr, cfg.ContentDir, cfg.IdleTimeout, cfg.RequestTimeout)

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start SIGHUP handler for certificate reload (Unix only, no-op on Windows)
	startCertReloader(cfg, prodMode)

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
				handleConn(conn, h, cfg.RequestTimeout)
			})
		}
	}()

	// Wait for shutdown signal or listener error
	select {
	case sig := <-sigChan:
		log.Printf("[INFO] received signal %v, initiating graceful shutdown", sig)
	case err := <-errChan:
		log.Printf("[ERROR] listener error: %v", err)
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
		log.Printf("[INFO] all connections drained")
	case <-time.After(10 * time.Second):
		log.Printf("[WARN] shutdown timeout: some connections did not finish")
	}

	log.Printf("[INFO] demarkus-server stopped")
}

func handleConn(conn *quic.Conn, h *handler.Handler, requestTimeout time.Duration) {
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return // connection closed
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

// loadTLS returns a TLS config based on the server configuration.
// In production mode (cert+key provided), uses GetCertificate callback
// so certificates can be reloaded at runtime via SIGHUP.
// If neither is set, generates a self-signed dev certificate.
// Returns an error if only one of cert/key is provided.
func loadTLS(cfg *config.Config) (tlsConfig *tls.Config, prodMode bool, err error) {
	haveCert := cfg.TLSCert != ""
	haveKey := cfg.TLSKey != ""

	switch {
	case haveCert && haveKey:
		log.Printf("[INFO] tls: loading certificate from %s", cfg.TLSCert)
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
		log.Printf("[INFO] tls: using self-signed dev certificate (set DEMARKUS_TLS_CERT and DEMARKUS_TLS_KEY for production)")
		tc, err := servertls.GenerateDevConfig()
		return tc, false, err
	}
}
