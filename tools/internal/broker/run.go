package broker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// RunOptions parameterizes the shared broker lifecycle: everything the
// two product binaries do not share (profile, validation, provisioning)
// is injected here so lifecycle fixes land once.
type RunOptions struct {
	// LogName prefixes every lifecycle log line ("broker", "memory broker").
	LogName string
	// Realm is the default WWW-Authenticate realm applied when the
	// config leaves Server.Realm blank.
	Realm string
	// Profile selects the MCP gateway surface.
	Profile *GatewayProfile
	// Validate runs extra config checks after LoadConfig.
	Validate []func(*Config) error
	// Setup runs after NewServer, before the listeners: optional extra
	// background tasks (sweep group) plus a cleanup Run invokes once on
	// every exit path; on error, Setup releases its own state first.
	Setup func(cfg *Config, srv *Server, log *slog.Logger) (background []func(ctx context.Context), cleanup func(), err error)
	// Version is the binary's build version (initialize response).
	Version string
	// KubeconfigPath selects an out-of-cluster kubeconfig; empty uses
	// the in-cluster service-account config.
	KubeconfigPath string
}

// Run is the shared broker main: config, auth machinery, both HTTP
// listeners, sweeper + device janitor, signal handling, and the
// two-phase shutdown. Blocks until shutdown completes.
func Run(configPath string, opts *RunOptions, log *slog.Logger) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	for _, validate := range opts.Validate {
		if err := validate(cfg); err != nil {
			return err
		}
	}
	if cfg.Server.Realm == "" {
		cfg.Server.Realm = opts.Realm
	}
	log.Info(opts.LogName+": config loaded",
		"addr", cfg.Server.Addr,
		"oidcIssuer", cfg.OIDC.Issuer,
		"worlds", len(cfg.Worlds),
		"version", opts.Version,
	)

	signer, err := NewSigner(cfg.Server.CookieKey)
	if err != nil {
		return err
	}

	// OIDC discovery is eager so a misconfigured broker fails to start
	// rather than failing the first user login. coreos/go-oidc builds
	// its own background context for JWKS refresh, so Background is fine.
	verifier, err := NewVerifier(context.Background(), &cfg.OIDC)
	if err != nil {
		return err
	}

	// Storage backend: kubernetes needs a client; file mode (single-host)
	// runs with no cluster at all.
	var store SecretStore
	var k8s kubernetes.Interface
	if cfg.FileBackend() {
		store = NewFileSecretStore()
		log.Info(opts.LogName+": file storage backend", "dir", cfg.Storage.Dir)
	} else {
		k8s, err = newKubeClient(opts.KubeconfigPath)
		if err != nil {
			return err
		}
		store = NewK8sSecretStore(k8s)
	}

	// Same eager-failure posture as NewVerifier; 5-minute TTL bounds
	// IdP key-rotation propagation.
	discovery, err := NewDiscovery(context.Background(), DiscoveryConfig{
		BrokerURL: cfg.Server.PublicURL,
		IdPIssuer: cfg.OIDC.Issuer,
		Log:       log,
	})
	if err != nil {
		return err
	}

	// Broker-side ECDSA signer for the refresh-grant path; an invalid
	// PEM fails the pod fast rather than the first refresh.
	idTokenSigner, err := NewIDTokenSigner([]byte(cfg.OIDC.BrokerSigningKey))
	if err != nil {
		return err
	}
	log.Info(opts.LogName+": id_token signer ready", "kid", idTokenSigner.KeyID())

	srv := NewServer(cfg, signer, verifier, store, discovery, idTokenSigner, log)

	// One deferred site owns Setup teardown on every path, including a
	// failed Setup that still handed back a cleanup.
	var background []func(ctx context.Context)
	cleanup := func() {}
	defer func() { cleanup() }() //nolint:gocritic // deliberate lambda: cleanup is reassigned by Setup after this defer
	if opts.Setup != nil {
		setupBackground, setupCleanup, setupErr := opts.Setup(cfg, srv, log)
		background = setupBackground
		if setupCleanup != nil {
			cleanup = setupCleanup
		}
		if setupErr != nil {
			return setupErr
		}
	}

	if cfg.RateLimit.Disabled {
		log.Info(opts.LogName + ": rate limit disabled (rateLimit.disabled=true)")
	} else {
		log.Info(opts.LogName+": rate limit enabled",
			"tokensPerMin", cfg.RateLimit.Tokens.PerMinute,
			"tokensBurst", cfg.RateLimit.Tokens.Burst,
			"loginPerMin", cfg.RateLimit.Login.PerMinute,
			"loginBurst", cfg.RateLimit.Login.Burst,
			"trustForwardedFor", cfg.RateLimit.TrustForwardedFor)
	}

	// Register the signal handler before any listener starts so a
	// SIGTERM in the startup window still takes the graceful path (the
	// buffered channel retains it until the select).
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errs := make(chan error, 1)
	go func() {
		log.Info(opts.LogName+": listening", "addr", cfg.Server.Addr)
		errs <- filterServerClosed(httpSrv.ListenAndServe())
	}()

	// MCP gateway on its own listener; distinct Addr lets the chart
	// route the two surfaces through different Ingress hosts or paths.
	mcpSrv := &http.Server{
		Addr:              cfg.Server.MCP.Addr,
		Handler:           srv.MCPGateway(opts.Version, opts.Profile),
		ReadHeaderTimeout: 10 * time.Second,
	}
	mcpErrs := make(chan error, 1)
	mcpTLS := cfg.Server.MCP.TLS
	go func() {
		log.Info(opts.LogName+": mcp gateway listening",
			"addr", cfg.Server.MCP.Addr, "tls", mcpTLS.CertFile != "")
		if mcpTLS.CertFile != "" {
			mcpErrs <- filterServerClosed(mcpSrv.ListenAndServeTLS(mcpTLS.CertFile, mcpTLS.KeyFile))
			return
		}
		mcpErrs <- filterServerClosed(mcpSrv.ListenAndServe())
	}()

	// Sweeper runs leader-elected across replicas (single-host file mode
	// skips the election); the device janitor is per-replica state. One
	// cancel tears down every background task before HTTP shutdown.
	sweepCtx, cancelSweep := context.WithCancel(context.Background())
	defer cancelSweep()
	var sweepWG sync.WaitGroup
	if !cfg.Sweeper.Disabled {
		sweeper := NewSweeper(k8s, srv.RefreshStore(), cfg.Sweeper.Interval, log)
		if cfg.FileBackend() {
			log.Info(opts.LogName+": starting sweeper (single-host)", "interval", cfg.Sweeper.Interval)
			sweepWG.Go(func() {
				sweeper.Run(sweepCtx)
			})
		} else {
			identity := brokerIdentity()
			log.Info(opts.LogName+": starting sweeper",
				"interval", cfg.Sweeper.Interval, "leaseName", cfg.Sweeper.LeaseName,
				"namespace", cfg.Server.BrokerNamespace, "identity", identity)
			sweepWG.Go(func() {
				sweeper.RunLeaderElected(sweepCtx, cfg.Sweeper.LeaseName, cfg.Server.BrokerNamespace, identity)
			})
		}
	} else {
		log.Info(opts.LogName + ": sweeper disabled (sweeper.disabled=true)")
	}

	log.Info(opts.LogName+": starting device-store janitor",
		"deviceCodeTTL", cfg.Server.DeviceCodeTTL,
		"devicePollInterval", cfg.Server.DevicePollInterval)
	sweepWG.Go(func() {
		srv.RunDeviceJanitor(sweepCtx)
	})
	for _, task := range background {
		sweepWG.Go(func() {
			task(sweepCtx)
		})
	}

	select {
	case sig := <-stop:
		log.Info(opts.LogName+": received signal, shutting down", "signal", sig.String())
	case err := <-errs:
		if err != nil {
			cancelSweep()
			sweepWG.Wait()
			return err
		}
		log.Warn(opts.LogName + ": management listener stopped, shutting down")
	case err := <-mcpErrs:
		if err != nil {
			cancelSweep()
			sweepWG.Wait()
			return err
		}
		log.Warn(opts.LogName + ": mcp gateway listener stopped, shutting down")
	}

	cancelSweep()
	sweepWG.Wait()

	// Separate shutdown contexts per surface so a slow MCP drain never
	// burns the management-API deadline; the management API's outcome is
	// the authoritative liveness signal.
	mcpShutdownCtx, cancelMCPShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelMCPShutdown()
	if err := mcpSrv.Shutdown(mcpShutdownCtx); err != nil {
		log.Warn(opts.LogName+": mcp gateway shutdown error", "err", err)
	}
	// Drain pooled QUIC connections after http.Shutdown so in-flight
	// tool calls have already returned.
	srv.CloseMCPGateway()
	mgmtShutdownCtx, cancelMgmtShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelMgmtShutdown()
	return httpSrv.Shutdown(mgmtShutdownCtx)
}

// filterServerClosed maps the graceful-shutdown sentinel to nil so the
// listener goroutines report only real failures.
func filterServerClosed(err error) error {
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// brokerIdentity is the leader-election holder identity: POD_NAME via
// the downward API in-cluster, hostname or a literal fallback elsewhere.
func brokerIdentity() string {
	if v := os.Getenv("POD_NAME"); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "broker"
}

// newKubeClient builds a kubernetes client: in-cluster service-account
// config when kubeconfigPath is empty, the given kubeconfig otherwise.
func newKubeClient(kubeconfigPath string) (kubernetes.Interface, error) {
	var cfg *rest.Config
	var err error
	if kubeconfigPath != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}
