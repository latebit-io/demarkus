package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/config"
	"github.com/latebit-io/demarkus/server/internal/configwatch"
	"github.com/latebit-io/demarkus/server/internal/handler"
	"github.com/latebit-io/demarkus/server/internal/logging"
	"github.com/latebit-io/demarkus/server/internal/ratelimit"
	servertls "github.com/latebit-io/demarkus/server/internal/tls"
	"github.com/quic-go/quic-go"
)

// currentWalker is the slice of a document store the catalog build needs.
type currentWalker interface {
	WalkCurrent(fn func(store.CurrentDoc) error) error
}

// buildCatalog builds the in-memory LOOKUP catalog by walking current
// documents, for backends that have no catalog of their own. A failed walk
// leaves an empty catalog rather than aborting startup.
func buildCatalog(s currentWalker, logger *slog.Logger) *catalog.Catalog {
	cat := catalog.New()
	err := s.WalkCurrent(func(d store.CurrentDoc) error {
		cat.Set(catalog.FromDocument(d.Path, d.Metadata, d.Body, d.Modified))
		return nil
	})
	if err != nil && !logPartialWalk(logger, "lookup catalog", err) {
		logger.Warn("lookup catalog build failed", "error", err)
		return cat
	}
	logger.Info("lookup catalog built", "entries", cat.Len())
	return cat
}

// logPartialWalk reports each entry a completed walk skipped and returns
// true; any other error returns false for the caller to handle.
func logPartialWalk(logger *slog.Logger, what string, err error) bool {
	var partial *store.PartialWalkError
	if !errors.As(err, &partial) {
		return false
	}
	for _, e := range partial.Skipped {
		logger.Warn(what+" skipped entry", "path", e.Path, "error", e.Err)
	}
	if partial.Total > len(partial.Skipped) {
		logger.Warn(what+" skipped more entries than listed", "total", partial.Total, "listed", len(partial.Skipped))
	}
	return true
}

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// flagOverrides holds the CLI flag values that override env-derived config.
type flagOverrides struct {
	root         string
	tlsCert      string
	tlsKey       string
	tokens       string
	storeBackend string
	pgDSN        string
	port         int
	readOnly     bool
}

// applyFlagOverrides applies non-empty/non-zero CLI flags onto cfg. Flags take
// precedence over env vars.
func applyFlagOverrides(cfg *config.Config, o *flagOverrides) {
	if o.root != "" {
		cfg.ContentDir = o.root
	}
	if o.port != 0 {
		cfg.Port = o.port
	}
	if o.tlsCert != "" {
		cfg.TLSCert = o.tlsCert
	}
	if o.tlsKey != "" {
		cfg.TLSKey = o.tlsKey
	}
	if o.tokens != "" {
		cfg.TokensFile = o.tokens
	}
	if o.storeBackend != "" {
		cfg.StoreBackend = o.storeBackend
	}
	if o.pgDSN != "" {
		cfg.PostgresDSN = o.pgDSN
	}
	if o.readOnly {
		cfg.ReadOnly = true
	}
}

func main() {
	root := flag.String("root", "", "content directory to serve (overrides DEMARKUS_ROOT)")
	port := flag.Int("port", 0, "port to listen on (overrides DEMARKUS_PORT)")
	tlsCert := flag.String("tls-cert", "", "path to TLS certificate PEM file (overrides DEMARKUS_TLS_CERT)")
	tlsKey := flag.String("tls-key", "", "path to TLS private key PEM file (overrides DEMARKUS_TLS_KEY)")
	tokens := flag.String("tokens", "", "path to TOML tokens file for auth (overrides DEMARKUS_TOKENS)")
	storeBackend := flag.String("store", "",
		fmt.Sprintf("document store backend: %s (overrides DEMARKUS_STORE)", strings.Join(storeNames(), " or ")))
	pgDSN := flag.String("pg-dsn", "", "Postgres connection string for the postgres backend (overrides DEMARKUS_PG_DSN)")
	readOnly := flag.Bool("read-only", false, "reject all write operations (also enabled via DEMARKUS_READ_ONLY)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: demarkus-server [options]\n\n")
		fmt.Fprintf(os.Stderr, "Serves markdown documents over the Mark Protocol (QUIC, port %d).\n", protocol.DefaultPort)
		fmt.Fprintf(os.Stderr, "Options can also be set via environment variables (DEMARKUS_ROOT, etc.).\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := config.NewConfig()

	// Create logger early so all subsequent output is structured.
	logger := logging.New(cfg.LogFormat, cfg.LogLevel, nil)

	// Fail-closed: running with silently-defaulted or invalid values (e.g. a
	// typo'd port or a rate limiter that rejects everything) is worse than
	// refusing to start.
	if err != nil {
		logger.Error("config invalid", "error", err)
		os.Exit(1)
	}

	applyFlagOverrides(cfg, &flagOverrides{
		root:         *root,
		port:         *port,
		tlsCert:      *tlsCert,
		tlsKey:       *tlsKey,
		tokens:       *tokens,
		storeBackend: *storeBackend,
		pgDSN:        *pgDSN,
		readOnly:     *readOnly,
	})
	// Semantic validation runs once, on the final post-override values.
	if err := cfg.Validate(); err != nil {
		logger.Error("configuration invalid", "error", err)
		os.Exit(1)
	}
	b, err := openStore(cfg, logger)
	if err != nil {
		logger.Error("store unavailable", "store", cfg.StoreBackend, "error", err)
		os.Exit(1)
	}
	if b.Close != nil {
		defer func() {
			if err := b.Close(); err != nil {
				logger.Warn("store close failed", "error", err)
			}
		}()
	}
	logger.Info("store ready", "backend", cfg.StoreBackend)
	docStore, cat := b.Store, b.Catalog

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

	if cfg.TokensFile != "" {
		if err := loadTokenStore(cfg.TokensFile); err != nil {
			logger.Error("token loading failed", "error", err)
			os.Exit(1)
		}
		logger.Info("auth: loaded tokens", "path", cfg.TokensFile)
		startTokenFileWatcher(cfg.TokensFile, logger)
	} else {
		logger.Info("auth: no tokens file configured, writes disabled")
	}

	h := &handler.Handler{
		Store:    docStore,
		Catalog:  cat,
		Logger:   logger,
		ReadOnly: cfg.ReadOnly,
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

	if cfg.ReadOnly {
		logger.Info("read-only mode enabled, all write operations will be rejected")
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

// writeRateLimited sends a rate-limited status response on the stream. Unlike a
// bare Close (which the client reads as an empty/statusless reply it cannot tell
// apart from a dead connection), this carries an explicit status the client can
// recognize and back off on.
func writeRateLimited(w io.Writer) error {
	_, err := protocol.Response{Status: protocol.StatusRateLimited}.WriteTo(w)
	return err
}

// maxRateWaitBudget caps how long a request may block on the rate limiter when
// no request timeout is configured (DEMARKUS_REQUEST_TIMEOUT of 0 or negative).
// An unbounded wait could pile up goroutines under a sustained flood.
const maxRateWaitBudget = 10 * time.Second

// handleConn accepts streams on a connection and dispatches each one to its own
// goroutine. The accept loop never blocks on the rate limiter — that work lives
// in serveStream — so a throttled request can't stall acceptance of other
// streams on the same connection.
func handleConn(conn *quic.Conn, h *handler.Handler, requestTimeout time.Duration, rl *ratelimit.Limiter, logger *slog.Logger) {
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return // connection closed
		}
		go serveStream(conn, stream, h, requestTimeout, rl, logger)
	}
}

// serveStream throttles a single request to the per-IP rate, then dispatches it
// to the handler. Over-limit requests are paced (not dropped) so bursty-but-
// legitimate clients keep working; a request that still can't be served within
// the wait budget gets an explicit rate-limited status response rather than an
// empty close (which a client cannot tell apart from a dead connection).
func serveStream(conn *quic.Conn, stream *quic.Stream, h *handler.Handler, requestTimeout time.Duration, rl *ratelimit.Limiter, logger *slog.Logger) {
	if rl != nil {
		ip := ratelimit.ExtractIP(conn.RemoteAddr())
		// Always bound the wait. requestTimeout may be 0/negative (timeout
		// disabled or misconfigured), so fall back to a fixed cap rather than
		// waiting forever.
		budget := requestTimeout
		if budget <= 0 {
			budget = maxRateWaitBudget
		}
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		err := rl.Wait(ctx, ip)
		cancel()
		if err != nil {
			logger.Warn("rate limited", "ip", ip, "error", err)
			// A failed write is the silent-close case we're trying to avoid (the
			// client gets nothing it can distinguish from a dead connection), so
			// surface it rather than swallowing it.
			if werr := writeRateLimited(stream); werr != nil {
				logger.Warn("writing rate-limited response", "ip", ip, "error", werr)
			}
			// Close error is non-actionable on an already-rejected stream.
			_ = stream.Close()
			return
		}
	}
	if requestTimeout > 0 {
		// SetReadDeadline only errors on a closed stream; HandleStream then fails
		// fast on its first read, so the error is non-actionable here.
		_ = stream.SetReadDeadline(time.Now().Add(requestTimeout))
	}
	h.HandleStream(stream)
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

// startTokenFileWatcher reloads the token store when the tokens file changes
// on disk. SIGHUP already covers operator-driven reloads; this complements it
// for environments where the file is rewritten in place without a signal.
func startTokenFileWatcher(path string, logger *slog.Logger) {
	w := &configwatch.Watcher{
		Target: path,
		Reload: func() error { return loadTokenStore(path) },
		Logger: logger,
	}
	go func() {
		if err := w.Run(context.Background()); err != nil {
			logger.Warn("auth: token file watcher exited", "error", err)
		}
	}()
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
