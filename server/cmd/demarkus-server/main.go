// demarkus-server serves one soul: the knowledge server's runtime in its
// local profile, configured by DEMARKUS_* env and flags, no YAML, no bucket,
// no account, no network beyond its own port.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/config"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
	"github.com/latebit-io/demarkus/server/internal/logging"
	"github.com/latebit-io/demarkus/server/knowledgeserver"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// open is the server over cfg; tests serve a soul through it the way run does.
func open(cfg *config.Config, logger *slog.Logger) (*knowledgeserver.Server, error) {
	profile := knowledgeconfig.LocalProfile(&knowledgeconfig.LocalOptions{
		Dir:               cfg.ContentDir,
		TokensFile:        cfg.TokensFile,
		ListenAddress:     net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.Port)),
		MaxStreams:        int64(cfg.MaxStreams),
		IdleTimeout:       knowledgeconfig.Duration(cfg.IdleTimeout),
		CertFile:          cfg.TLSCert,
		KeyFile:           cfg.TLSKey,
		ReadOnly:          cfg.ReadOnly,
		RequestTimeout:    knowledgeconfig.Duration(cfg.RequestTimeout),
		RequestsPerSecond: cfg.RateLimit,
		Burst:             cfg.RateBurst,
		MaxWatches:        cfg.MaxWatches,
	})
	return knowledgeserver.Open(knowledgeserver.Options{Config: profile, Logger: logger})
}

func main() {
	// Cleanup lives in run's defers; os.Exit here is the only exit after
	// they have run.
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	cfg, envErr := config.NewConfig()
	// Flags default to the env-derived values, so a flag given wins.
	flag.StringVar(&cfg.ContentDir, "root", cfg.ContentDir, "content directory to serve (overrides DEMARKUS_ROOT)")
	flag.IntVar(&cfg.Port, "port", cfg.Port, "port to listen on (overrides DEMARKUS_PORT)")
	flag.StringVar(&cfg.ListenHost, "listen", cfg.ListenHost, "interface to bind, e.g. 127.0.0.1; empty binds every one (overrides DEMARKUS_LISTEN)")
	flag.StringVar(&cfg.TLSCert, "tls-cert", cfg.TLSCert, "path to TLS certificate PEM file (overrides DEMARKUS_TLS_CERT)")
	flag.StringVar(&cfg.TLSKey, "tls-key", cfg.TLSKey, "path to TLS private key PEM file (overrides DEMARKUS_TLS_KEY)")
	flag.StringVar(&cfg.TokensFile, "tokens", cfg.TokensFile, "path to TOML tokens file for auth (overrides DEMARKUS_TOKENS)")
	flag.StringVar(&cfg.StoreBackend, "store", cfg.StoreBackend, "document store backend: file (overrides DEMARKUS_STORE)")
	flag.BoolVar(&cfg.ReadOnly, "read-only", cfg.ReadOnly, "reject all write operations (also enabled via DEMARKUS_READ_ONLY)")
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

	// Create logger early so all subsequent output is structured.
	logger := logging.New(cfg.LogFormat, cfg.LogLevel, nil)
	// Fail-closed: running with silently-defaulted or invalid values (e.g. a
	// typo'd port or a rate limiter that rejects everything) is worse than
	// refusing to start.
	if envErr != nil {
		logger.Error("config invalid", "error", envErr)
		return envErr
	}
	// Semantic validation runs once, on the final post-override values.
	if err := cfg.Validate(); err != nil {
		logger.Error("configuration invalid", "error", err)
		return err
	}
	if cfg.TokensFile == "" {
		logger.Info("auth: no tokens file configured, writes disabled")
	}
	if cfg.ReadOnly {
		logger.Info("read-only mode enabled, all write operations will be rejected")
	}
	logger.Info("serving soul", "root", cfg.ContentDir, "port", cfg.Port)

	server, err := open(cfg, logger)
	if err != nil {
		return err
	}
	defer server.Close()
	ctx, stop := server.WatchSignals(context.Background())
	defer stop()
	return server.Serve(ctx)
}
