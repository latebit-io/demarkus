package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/certsource"
	"github.com/latebit-io/demarkus/server/internal/configwatch"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob/gcs"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
	"github.com/latebit-io/demarkus/server/internal/logging"
	"github.com/latebit-io/demarkus/server/internal/management"
	"github.com/latebit-io/demarkus/server/internal/quicserve"
	"github.com/quic-go/quic-go"
)

const (
	maxObjectBytes  = 4 << 20
	shutdownTimeout = 10 * time.Second
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("demarkus-knowledge-server", flag.ContinueOnError)
	configFile := flags.String("config", "", "path to strict multi-world YAML configuration")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *configFile == "" {
		return errors.New("-config is required")
	}

	logger := logging.New("json", "info", nil)
	config, err := knowledgeconfig.Load(*configFile)
	if err != nil {
		logger.Error("configuration invalid", "error", err)
		return err
	}
	// Dynamic mode (worldsFile set) skips authority pinning on the cert:
	// tenant worlds come and go at runtime, so coverage is checked
	// per-world with a warning instead of failing cert reloads.
	authorities := configuredAuthorities(config)
	if config.WorldsFile != "" {
		authorities = nil
	}
	certificates, err := certsource.Open(config.TLS.CertFile, config.TLS.KeyFile, authorities)
	if err != nil {
		logger.Error("TLS setup failed", "error", err)
		return err
	}

	startupCtx, startupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer startupCancel()
	client, err := storage.NewClient(startupCtx)
	if err != nil {
		logger.Error("GCS client unavailable", "error", err)
		return err
	}
	defer func() {
		if err := client.Close(); err != nil {
			logger.Warn("GCS client close failed", "error", err)
		}
	}()

	watchCtx, stopWatchers := context.WithCancel(context.Background())
	var watcherGroup sync.WaitGroup
	defer func() {
		stopWatchers()
		watcherGroup.Wait()
	}()

	newStore := func(_ context.Context, world *knowledgeconfig.WorldConfig) (blob.Store, error) {
		return gcs.New(client, world.Bucket.Name(), maxObjectBytes)
	}
	worlds, err := newWorldManager(watchCtx, &watcherGroup, *configFile, config, newStore, certificates, logger)
	if err != nil {
		logger.Error("world startup failed", "error", err)
		return err
	}
	defer worlds.Close()
	tokens := worlds.Tokens()
	router := worlds.Router()
	tlsConfig := certificates.TLSConfig(protocol.ALPN)
	handshakeHook, err := router.HandshakeHook(tlsConfig)
	if err != nil {
		return err
	}
	tlsConfig.GetConfigForClient = handshakeHook

	quicServer, err := quicserve.Listen(quicserve.Config{
		Address:   config.Listen.Address,
		TLSConfig: tlsConfig,
		QUICConfig: &quic.Config{
			MaxIncomingStreams:    config.Listen.MaxIncomingStreams,
			MaxIncomingUniStreams: 0,
			MaxIdleTimeout:        time.Duration(config.Listen.IdleTimeout),
		},
		Logger: logger,
	})
	if err != nil {
		logger.Error("QUIC listen failed", "error", err)
		return err
	}
	defer func() {
		if err := quicServer.Close(); err != nil {
			logger.Warn("QUIC close failed", "error", err)
		}
	}()

	healthListener, err := net.Listen("tcp", config.Health.Address)
	if err != nil {
		logger.Error("health listen failed", "error", err)
		return err
	}
	health := &management.Health{}
	healthServer := &http.Server{
		Handler:           health.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	defer func() {
		if err := healthServer.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("health server close failed", "error", err)
		}
	}()

	startConfigWatchers(watchCtx, &watcherGroup, *configFile, config, worlds, logger)

	quicResult := make(chan error, 1)
	go func() { quicResult <- quicServer.Serve(context.Background(), router.Selector()) }()
	healthResult := make(chan error, 1)
	go func() { healthResult <- healthServer.Serve(healthListener) }()
	health.SetLive(true)
	health.SetReady(true)
	logger.Info("knowledge server started", "quic_addr", quicServer.Addr(), "health_addr", healthListener.Addr(), "worlds", worlds.WorldCount())

	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, processSignals()...)
	defer signal.Stop(signalChannel)
	runErr := waitForStop(signalChannel, quicResult, healthResult, certificates, tokens, logger)

	health.SetReady(false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	if err := quicServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("QUIC shutdown incomplete", "error", err)
	}
	cancel()
	health.SetLive(false)
	healthCtx, healthCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := healthServer.Shutdown(healthCtx); err != nil {
		logger.Warn("health shutdown incomplete", "error", err)
	}
	healthCancel()
	logger.Info("knowledge server stopped")
	return runErr
}

func configuredAuthorities(config *knowledgeconfig.Config) []string {
	count := 0
	for worldIndex := range config.Worlds {
		count += len(config.Worlds[worldIndex].Authorities)
	}
	authorities := make([]string, 0, count)
	for worldIndex := range config.Worlds {
		world := &config.Worlds[worldIndex]
		authorities = append(authorities, world.Authorities...)
	}
	return authorities
}

func waitForStop(
	signals <-chan os.Signal,
	quicResult <-chan error,
	healthResult <-chan error,
	certificates *certsource.Source,
	tokens *tokenCoordinator,
	logger *slog.Logger,
) error {
	for {
		select {
		case received := <-signals:
			if isReloadSignal(received) {
				if err := certificates.Reload(); err != nil {
					logger.Error("TLS certificate reload failed", "error", err)
				} else {
					logger.Info("TLS certificate reloaded")
				}
				if err := tokens.Reload(); err != nil {
					logger.Error("token reload failed", "error", err)
				} else {
					logger.Info("tokens reloaded")
				}
				continue
			}
			logger.Info("received shutdown signal", "signal", received.String())
			return nil
		case err := <-quicResult:
			if errors.Is(err, quicserve.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("QUIC server stopped: %w", err)
		case err := <-healthResult:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("health server stopped: %w", err)
		}
	}
}

// startConfigWatchers reloads the world set when the main config or the
// worldsFile fragment changes on disk (both are typically projected
// ConfigMaps whose updates arrive as atomic symlink swaps).
func startConfigWatchers(
	ctx context.Context,
	group *sync.WaitGroup,
	configFile string,
	config *knowledgeconfig.Config,
	worlds *worldManager,
	logger *slog.Logger,
) {
	targets := []string{configFile}
	if fragment := config.WorldsFilePath(configFile); fragment != "" {
		targets = append(targets, fragment)
	}
	for _, target := range targets {
		watcher := &configwatch.Watcher{Target: target, Reload: worlds.Reload, Logger: logger}
		group.Go(func() {
			if err := watcher.Run(ctx); err != nil {
				logger.Warn("config watcher exited", "target", watcher.Target, "error", err)
			}
		})
	}
}
