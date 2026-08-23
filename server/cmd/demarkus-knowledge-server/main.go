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
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob/gcs"
	"github.com/latebit-io/demarkus/server/internal/knowledge/bucketstore"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
	"github.com/latebit-io/demarkus/server/internal/logging"
	"github.com/latebit-io/demarkus/server/internal/management"
	"github.com/latebit-io/demarkus/server/internal/quicserve"
	"github.com/latebit-io/demarkus/server/internal/snirouter"
	"github.com/latebit-io/demarkus/server/internal/worldruntime"
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
	authorities := configuredAuthorities(config)
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

	runtimes, mappings, tokenWorlds, err := openWorlds(startupCtx, config, client, logger)
	if err != nil {
		logger.Error("world startup failed", "error", err)
		return err
	}
	defer closeRuntimes(runtimes, logger)
	tokens, err := newTokenCoordinator(tokenWorlds)
	if err != nil {
		logger.Error("token isolation failed", "error", err)
		return err
	}
	router, err := snirouter.New(mappings)
	if err != nil {
		logger.Error("SNI router invalid", "error", err)
		return err
	}
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

	watchCtx, stopWatchers := context.WithCancel(context.Background())
	var watcherGroup sync.WaitGroup
	startTokenWatchers(watchCtx, &watcherGroup, config, tokens, logger)
	defer func() {
		stopWatchers()
		watcherGroup.Wait()
	}()

	quicResult := make(chan error, 1)
	go func() { quicResult <- quicServer.Serve(context.Background(), router.Selector()) }()
	healthResult := make(chan error, 1)
	go func() { healthResult <- healthServer.Serve(healthListener) }()
	health.SetLive(true)
	health.SetReady(true)
	logger.Info("knowledge server started", "quic_addr", quicServer.Addr(), "health_addr", healthListener.Addr(), "worlds", len(runtimes))

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

func openWorlds(
	ctx context.Context,
	config *knowledgeconfig.Config,
	client *storage.Client,
	logger *slog.Logger,
) ([]*worldruntime.Runtime, []snirouter.Mapping, []tokenWorld, error) {
	runtimes := make([]*worldruntime.Runtime, 0, len(config.Worlds))
	mappings := make([]snirouter.Mapping, 0)
	tokenWorlds := make([]tokenWorld, 0, len(config.Worlds))
	for worldIndex := range config.Worlds {
		world := &config.Worlds[worldIndex]
		objects, err := gcs.New(client, world.Bucket.Name(), maxObjectBytes)
		if err != nil {
			closeRuntimes(runtimes, logger)
			return nil, nil, nil, fmt.Errorf("open world %q blob store: %w", world.Name, err)
		}
		store, err := bucketstore.Open(ctx, objects, bucketstore.Options{
			WorldID:        world.Bucket.WorldID,
			RequestTimeout: time.Duration(world.Limits.RequestTimeout),
			RequirePolicy:  true,
		})
		if err != nil {
			closeRuntimes(runtimes, logger)
			return nil, nil, nil, fmt.Errorf("open world %q bucket: %w", world.Name, err)
		}
		runtime, err := worldruntime.New(&worldruntime.Config{
			Name:              world.Name,
			Store:             store,
			Catalog:           store,
			Views:             store,
			TokensFile:        world.Auth.TokensFile,
			DisableTokenWatch: true,
			ReadOnly:          world.ReadOnly,
			RequestTimeout:    time.Duration(world.Limits.RequestTimeout),
			MaxConcurrent:     world.Limits.MaxConcurrentRequests,
			RateLimit:         world.Limits.RequestsPerSecond,
			RateBurst:         world.Limits.Burst,
			Logger:            logger,
		})
		if err != nil {
			closeRuntimes(runtimes, logger)
			return nil, nil, nil, fmt.Errorf("open world %q runtime: %w", world.Name, err)
		}
		runtimes = append(runtimes, runtime)
		tokenWorlds = append(tokenWorlds, tokenWorld{name: world.Name, runtime: runtime})
		for _, authority := range world.Authorities {
			mappings = append(mappings, snirouter.Mapping{Authority: authority, Endpoint: runtime.Endpoint(authority)})
		}
	}
	return runtimes, mappings, tokenWorlds, nil
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

func startTokenWatchers(
	ctx context.Context,
	group *sync.WaitGroup,
	config *knowledgeconfig.Config,
	tokens *tokenCoordinator,
	logger *slog.Logger,
) {
	for worldIndex := range config.Worlds {
		world := &config.Worlds[worldIndex]
		watcher := &configwatch.Watcher{Target: world.Auth.TokensFile, Reload: tokens.Reload, Logger: logger}
		group.Go(func() {
			if err := watcher.Run(ctx); err != nil {
				logger.Warn("token watcher exited", "target", watcher.Target, "error", err)
			}
		})
	}
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

func closeRuntimes(runtimes []*worldruntime.Runtime, logger *slog.Logger) {
	for index := len(runtimes) - 1; index >= 0; index-- {
		if err := runtimes[index].Close(); err != nil {
			logger.Warn("world runtime close failed", "error", err)
		}
	}
}
