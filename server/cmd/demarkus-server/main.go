package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/config"
	"github.com/latebit-io/demarkus/server/internal/logging"
	"github.com/latebit-io/demarkus/server/internal/quicserve"
	servertls "github.com/latebit-io/demarkus/server/internal/tls"
	"github.com/latebit-io/demarkus/server/internal/worldruntime"
	"github.com/quic-go/quic-go"
)

// currentWalker is the slice of a document store the catalog build needs.
type currentWalker interface {
	WalkCurrent(fn func(store.CurrentDoc) error) error
}

type tokenReloader interface {
	ReloadTokens() error
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
	if o.readOnly {
		cfg.ReadOnly = true
	}
}

func main() {
	// Cleanup lives in run's defers; os.Exit here is the only exit after
	// they have run.
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	root := flag.String("root", "", "content directory to serve (overrides DEMARKUS_ROOT)")
	port := flag.Int("port", 0, "port to listen on (overrides DEMARKUS_PORT)")
	tlsCert := flag.String("tls-cert", "", "path to TLS certificate PEM file (overrides DEMARKUS_TLS_CERT)")
	tlsKey := flag.String("tls-key", "", "path to TLS private key PEM file (overrides DEMARKUS_TLS_KEY)")
	tokens := flag.String("tokens", "", "path to TOML tokens file for auth (overrides DEMARKUS_TOKENS)")
	storeBackend := flag.String("store", "",
		fmt.Sprintf("document store backend: %s (overrides DEMARKUS_STORE)", strings.Join(storeNames(), " or ")))
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
		return nil
	}

	cfg, err := config.NewConfig()

	// Create logger early so all subsequent output is structured.
	logger := logging.New(cfg.LogFormat, cfg.LogLevel, nil)

	// Fail-closed: running with silently-defaulted or invalid values (e.g. a
	// typo'd port or a rate limiter that rejects everything) is worse than
	// refusing to start.
	if err != nil {
		logger.Error("config invalid", "error", err)
		return err
	}

	applyFlagOverrides(cfg, &flagOverrides{
		root:         *root,
		port:         *port,
		tlsCert:      *tlsCert,
		tlsKey:       *tlsKey,
		tokens:       *tokens,
		storeBackend: *storeBackend,
		readOnly:     *readOnly,
	})
	// Semantic validation runs once, on the final post-override values.
	if err := cfg.Validate(); err != nil {
		logger.Error("configuration invalid", "error", err)
		return err
	}
	b, err := openStore(cfg, logger)
	if err != nil {
		logger.Error("store unavailable", "store", cfg.StoreBackend, "error", err)
		return err
	}
	backendOwned := true
	if b.Close != nil {
		defer func() {
			if backendOwned {
				if err := b.Close(); err != nil {
					logger.Warn("store close failed", "error", err)
				}
			}
		}()
	}
	logger.Info("store ready", "backend", cfg.StoreBackend)

	tlsConfig, prodMode, err := loadTLS(cfg, logger)
	if err != nil {
		logger.Error("tls setup failed", "error", err)
		return err
	}

	quicConfig := &quic.Config{
		MaxIncomingStreams:    int64(cfg.MaxStreams),
		MaxIncomingUniStreams: 0,
		MaxIdleTimeout:        cfg.IdleTimeout,
	}

	runtime, err := worldruntime.New(&worldruntime.Config{
		Store:          b.Store,
		Catalog:        b.Catalog,
		Views:          b.Views,
		CloseBackend:   b.Close,
		TokensFile:     cfg.TokensFile,
		ReadOnly:       cfg.ReadOnly,
		RequestTimeout: cfg.RequestTimeout,
		RateLimit:      cfg.RateLimit,
		RateBurst:      cfg.RateBurst,
		Logger:         logger,
	})
	if err != nil {
		logger.Error("world runtime unavailable", "error", err)
		return err
	}
	backendOwned = false
	defer func() {
		if err := runtime.Close(); err != nil {
			logger.Warn("world runtime close failed", "error", err)
		}
	}()
	if cfg.TokensFile != "" {
		logger.Info("auth: loaded tokens", "path", cfg.TokensFile)
	} else {
		logger.Info("auth: no tokens file configured, writes disabled")
	}
	if cfg.RateLimit > 0 {
		logger.Info("rate limit configured", "req_per_sec", cfg.RateLimit, "burst", cfg.RateBurst)
	}
	if cfg.ReadOnly {
		logger.Info("read-only mode enabled, all write operations will be rejected")
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	server, err := quicserve.Listen(quicserve.Config{
		Address:    addr,
		TLSConfig:  tlsConfig,
		QUICConfig: quicConfig,
		Logger:     logger,
	})
	if err != nil {
		logger.Error("listen failed", "addr", addr, "error", err)
		return err
	}
	defer func() {
		if err := server.Close(); err != nil {
			logger.Warn("server close failed", "error", err)
		}
	}()

	logger.Info("server started",
		"addr", server.Addr().String(),
		"root", cfg.ContentDir,
		"idle_timeout", cfg.IdleTimeout.String(),
		"request_timeout", cfg.RequestTimeout.String())

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	// Start SIGHUP handler for certificate reload (Unix only, no-op on Windows)
	startCertReloader(cfg, prodMode, runtime, logger)

	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(context.Background(), func(*quic.Conn) (quicserve.Endpoint, error) {
			return runtime, nil
		})
	}()

	var serveErr error
	select {
	case sig := <-sigChan:
		logger.Info("received signal, initiating graceful shutdown", "signal", sig.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Warn("server shutdown incomplete", "error", err)
		} else {
			logger.Info("all connections drained")
		}
		cancel()
		serveErr = <-serveResult
	case serveErr = <-serveResult:
		if !errors.Is(serveErr, quicserve.ErrServerClosed) {
			logger.Error("listener error", "error", serveErr)
		}
	}

	logger.Info("server stopped")
	if serveErr != nil && !errors.Is(serveErr, quicserve.ErrServerClosed) {
		return serveErr
	}
	return nil
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

// loadTLS configures reloadable production certificates or a self-signed
// development certificate. A partial production certificate pair is invalid.
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
