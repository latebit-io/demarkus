package knowledgeserver

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
	"github.com/latebit-io/demarkus/server/internal/worldruntime"
	"github.com/quic-go/quic-go"
)

const (
	maxObjectBytes  = 4 << 20
	shutdownTimeout = 10 * time.Second
)

// Options configures Open.
type Options struct {
	// ConfigFile is the strict multi-world YAML configuration.
	ConfigFile string
	// Logger defaults to JSON at info.
	Logger *slog.Logger
}

// Server is an opened knowledge server: worlds, peers and listeners, served
// by Serve and torn down by Close.
type Server struct {
	config       *knowledgeconfig.Config
	configFile   string
	logger       *slog.Logger
	certificates *certsource.Source
	gcs          *storage.Client
	watchCtx     context.Context
	stopWatchers context.CancelFunc
	watcherGroup sync.WaitGroup
	peers        *peerLinks
	worlds       *worldManager
	// listeners are the QUIC listeners: 6309 first, then the bearer
	// listener once opened.
	listeners  []quicListener
	bearerOpen bool
	health     *healthEndpoint
	closeOnce  sync.Once
}

// quicListener is one QUIC listener and the selector its connections go through.
type quicListener struct {
	name     string
	server   *quicserve.Server
	selector quicserve.Selector
}

// serve runs the listener; a stop by Shutdown or Close is nil.
func (l quicListener) serve() error {
	if err := l.server.Serve(context.Background(), l.selector); !errors.Is(err, quicserve.ErrServerClosed) {
		return fmt.Errorf("%s stopped: %w", l.name, err)
	}
	return nil
}

// Run is the knowledge server main: flags, Open, signals and Serve. Blocks
// until shutdown completes. version is the binary's build version.
func Run(arguments []string, version string) error {
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
	server, err := Open(Options{ConfigFile: *configFile})
	if err != nil {
		return err
	}
	defer server.Close()
	ctx, stop := server.WatchSignals(context.Background())
	defer stop()
	return server.Serve(ctx)
}

// WatchSignals returns a context that ends on a shutdown signal, and
// reloads the server on the platform's reload signal. stop releases the
// handler; a composed binary shares it with the server.
func (s *Server) WatchSignals(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, processSignals()...)
	go func() {
		defer signal.Stop(signals)
		for {
			select {
			case <-ctx.Done():
				return
			case received := <-signals:
				if isReloadSignal(received) {
					s.Reload()
					continue
				}
				s.logger.Info("received shutdown signal", "signal", received.String())
				cancel()
				return
			}
		}
	}()
	return ctx, cancel
}

// Open loads the configuration and opens TLS, the bucket client, peers,
// every world and both listeners. Nothing is served until Serve.
func Open(opts Options) (*Server, error) {
	logger := opts.Logger
	if logger == nil {
		logger = logging.New("json", "info", nil)
	}
	config, err := knowledgeconfig.Load(opts.ConfigFile)
	if err != nil {
		logger.Error("configuration invalid", "error", err)
		return nil, err
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
		return nil, err
	}
	s := &Server{config: config, configFile: opts.ConfigFile, logger: logger, certificates: certificates}
	s.watchCtx, s.stopWatchers = context.WithCancel(context.Background())
	if err := s.open(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// open fills in everything after TLS; Close on the partial Server undoes
// whatever it reached.
func (s *Server) open() error {
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer startupCancel()
	client, err := storage.NewClient(startupCtx)
	if err != nil {
		s.logger.Error("GCS client unavailable", "error", err)
		return err
	}
	s.gcs = client

	newStore := func(_ context.Context, world *knowledgeconfig.WorldConfig) (blob.Store, error) {
		return gcs.New(client, world.Bucket.Name(), maxObjectBytes)
	}
	peers, err := openPeers(s.config.Peers, s.certificates, s.logger)
	if err != nil {
		s.logger.Error("peer hints unavailable", "error", err)
		return err
	}
	s.peers = peers
	worlds, err := newWorldManager(s.watchCtx, &s.watcherGroup, worldManagerConfig{
		configFile: s.configFile, config: s.config, newStore: newStore, certs: s.certificates, peers: peers, logger: s.logger,
	})
	if err != nil {
		s.logger.Error("world startup failed", "error", err)
		return err
	}
	s.worlds = worlds

	quicServer, err := s.listenQUIC(s.config.Listen.Address)
	if err != nil {
		s.logger.Error("QUIC listen failed", "error", err)
		return err
	}
	s.listeners = append(s.listeners, quicListener{name: "QUIC server", server: quicServer, selector: worlds.Router().Selector()})

	health, err := openHealth(s.config.Health.Address)
	if err != nil {
		s.logger.Error("health listen failed", "error", err)
		return err
	}
	s.health = health
	return nil
}

// listenQUIC opens a mark listener on address: the worlds' certificate,
// strict SNI routing, the configured stream and idle limits.
func (s *Server) listenQUIC(address string) (*quicserve.Server, error) {
	tlsConfig := s.certificates.TLSConfig(protocol.ALPN)
	handshakeHook, err := s.worlds.Router().HandshakeHook(tlsConfig)
	if err != nil {
		return nil, err
	}
	tlsConfig.GetConfigForClient = handshakeHook
	return quicserve.Listen(quicserve.Config{
		Address:   address,
		TLSConfig: tlsConfig,
		QUICConfig: &quic.Config{
			MaxIncomingStreams:    s.config.Listen.MaxIncomingStreams,
			MaxIncomingUniStreams: 0,
			MaxIdleTimeout:        time.Duration(s.config.Listen.IdleTimeout),
		},
		Logger: s.logger,
	})
}

// Gates builds each bearer connection's protocol.Gate from its authority.
type Gates = worldruntime.Gates

// OpenBearerListener listens on address for mark clients with an identity
// bearer in the auth field; each connection's gate admits every request,
// and the handler never sees the bearer. Call before Serve, which runs it.
func (s *Server) OpenBearerListener(address string, gates Gates) error {
	if s.bearerOpen {
		return errors.New("bearer listener already open")
	}
	if gates == nil {
		return errors.New("bearer listener needs gates")
	}
	listener, err := s.listenQUIC(address)
	if err != nil {
		return fmt.Errorf("bearer listener: %w", err)
	}
	s.listeners = append(s.listeners, quicListener{name: "bearer listener", server: listener, selector: gatedSelector(s.worlds.Router().Selector(), gates)})
	s.bearerOpen = true
	s.logger.Info("bearer listener open", "addr", listener.Addr())
	return nil
}

// gatedSelector pins each connection to its world's endpoint behind the
// gate gates builds for the connection.
func gatedSelector(route quicserve.Selector, gates Gates) quicserve.Selector {
	return func(conn *quic.Conn) (quicserve.Endpoint, error) {
		endpoint, err := route(conn)
		if err != nil {
			return nil, err
		}
		gatable, ok := endpoint.(worldruntime.Gatable)
		if !ok {
			return nil, fmt.Errorf("endpoint %T serves no bearer connections", endpoint)
		}
		return gatable.Gated(gates)
	}
}

// Serve starts the config watchers and serves QUIC and health until ctx
// ends or a listener fails, then drains the worlds and stops the
// listeners. Close still has to run afterwards.
func (s *Server) Serve(ctx context.Context) error {
	startConfigWatchers(s.watchCtx, &s.watcherGroup, s.configFile, s.config, s.worlds, s.logger)

	quicResult := make(chan error, len(s.listeners))
	for _, listener := range s.listeners {
		go func() { quicResult <- listener.serve() }()
	}
	healthResult := make(chan error, 1)
	go func() { healthResult <- s.health.server.Serve(s.health.listener) }()
	s.health.status.SetLive(true)
	s.health.status.SetReady(true)
	s.logger.Info("knowledge server started", "quic_addr", s.listeners[0].server.Addr(), "health_addr", s.health.listener.Addr(), "worlds", s.worlds.WorldCount())

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-quicResult:
	case err := <-healthResult:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("health server stopped: %w", err)
		}
	}

	s.health.status.SetReady(false)
	s.shutdownListeners()
	s.health.status.SetLive(false)
	healthCtx, healthCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s.health.server.Shutdown(healthCtx); err != nil {
		s.logger.Warn("health shutdown incomplete", "error", err)
	}
	healthCancel()
	s.logger.Info("knowledge server stopped")
	return runErr
}

// shutdownListeners drains the worlds once, which ends every watch, then
// stops the QUIC listeners together, each under the shutdown timeout.
func (s *Server) shutdownListeners() {
	s.worlds.Drain()
	var group sync.WaitGroup
	for _, listener := range s.listeners {
		group.Go(func() { listener.server.ShutdownAfter(func() {}, shutdownTimeout) })
	}
	group.Wait()
}

// Reload re-reads the TLS certificate and every world's token files; the
// SIGHUP path. Failures are logged, the previous state stays live.
func (s *Server) Reload() {
	if err := s.certificates.Reload(); err != nil {
		s.logger.Error("TLS certificate reload failed", "error", err)
	} else {
		s.logger.Info("TLS certificate reloaded")
	}
	if err := s.worlds.Tokens().Reload(); err != nil {
		s.logger.Error("token reload failed", "error", err)
	} else {
		s.logger.Info("tokens reloaded")
	}
}

// ApplyWorldsFragment puts a worlds fragment in force ahead of the mounted
// worldsFile, so a tenant provisioned in this process serves at once.
func (s *Server) ApplyWorldsFragment(fragment []byte) error {
	return s.worlds.SetDynamicWorlds(fragment)
}

// Close tears down in reverse: reloaders and the retry loop first, then
// peers, so nothing reopens or hints a world while it closes; then the
// listeners and the bucket client. Safe after Serve and on a partial open.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.stopWatchers()
		s.watcherGroup.Wait()
		if s.peers != nil {
			s.peers.close(s.logger)
		}
		if s.worlds != nil {
			s.worlds.Close()
		}
		for _, listener := range s.listeners {
			if err := listener.server.Close(); err != nil {
				s.logger.Warn(listener.name+" close failed", "error", err)
			}
		}
		if s.health != nil {
			s.health.close(s.logger)
		}
		if s.gcs != nil {
			if err := s.gcs.Close(); err != nil {
				s.logger.Warn("GCS client close failed", "error", err)
			}
		}
	})
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
		watcher := &configwatch.Watcher{Targets: []string{target}, Reload: worlds.Reload, Logger: logger}
		group.Go(func() {
			if err := watcher.Run(ctx); err != nil {
				logger.Warn("config watcher exited", "target", target, "error", err)
			}
		})
	}
}

// healthEndpoint is the private management listener and its state.
type healthEndpoint struct {
	listener net.Listener
	server   *http.Server
	status   *management.Health
}

func openHealth(address string) (*healthEndpoint, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	status := &management.Health{}
	server := &http.Server{
		Handler:           status.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	return &healthEndpoint{listener: listener, server: server, status: status}, nil
}

func (h *healthEndpoint) close(logger *slog.Logger) {
	if err := h.server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Warn("health server close failed", "error", err)
	}
}
