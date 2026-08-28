// demarkus-memory-broker is memory as a service for MCP hosts: the
// OAuth-fronted MCP gateway that gives each identity a private,
// versioned personal soul. Built over the same shared broker libraries
// as demarkus-knowledge-broker, with a different authorization model:
// identity maps to exactly one world, and reads AND writes are locked
// to the caller's own world. Nothing is org-open; cross-tenant access
// is denied at the tool, resource, and crawl layers.
//
// Phase 2 scope (memory-broker plan): static hand-provisioned tenants
// (worlds in YAML, one identity per world via allow), the mark_* tool
// surface minus federation and multi-world listing, soul template
// seeding on first access, and server instructions plus soul-context /
// soul-journal MCP prompts so plugin-less hosts (Claude Desktop,
// ChatGPT) carry their guidance in the endpoint. Dynamic tenant
// provisioning is Phase 3.
package main

import (
	"context"
	"errors"
	"flag"
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

	"github.com/latebit-io/demarkus/tools/internal/broker"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/demarkus-memory-broker/config.yaml", "path to broker YAML config")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig (default: in-cluster config)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		_, _ = os.Stdout.WriteString(version + "\n")
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(*configPath, *kubeconfig, log); err != nil {
		log.Error("memory broker exited with error", "err", err)
		os.Exit(1)
	}
}

func run(configPath, kubeconfigPath string, log *slog.Logger) error {
	cfg, err := broker.LoadConfig(configPath)
	if err != nil {
		return err
	}
	// Memory-broker invariant on top of the shared validation: every
	// world names its tenant, otherwise identity = world is ambiguous.
	if err := cfg.ValidateTenantWorlds(); err != nil {
		return err
	}
	if cfg.Server.Realm == "" {
		cfg.Server.Realm = "demarkus-memory-broker"
	}
	log.Info("memory broker: config loaded",
		"addr", cfg.Server.Addr,
		"oidcIssuer", cfg.OIDC.Issuer,
		"worlds", len(cfg.Worlds),
		"version", version,
	)

	signer, err := broker.NewSigner(cfg.Server.CookieKey)
	if err != nil {
		return err
	}

	// OIDC discovery is performed eagerly so a misconfigured broker fails
	// to start rather than failing the first user login (see the
	// knowledge broker's main for the context rationale).
	verifier, err := broker.NewVerifier(context.Background(), &cfg.OIDC)
	if err != nil {
		return err
	}

	// Storage backend: kubernetes needs a client; file mode (single-host)
	// runs with no cluster at all.
	var store broker.SecretStore
	var k8s kubernetes.Interface
	if cfg.FileBackend() {
		store = broker.NewFileSecretStore()
		log.Info("memory broker: file storage backend", "dir", cfg.Storage.Dir)
	} else {
		k8s, err = newKubeClient(kubeconfigPath)
		if err != nil {
			return err
		}
		store = broker.NewK8sSecretStore(k8s)
	}

	discovery, err := broker.NewDiscovery(context.Background(), broker.DiscoveryConfig{
		BrokerURL: cfg.Server.PublicURL,
		IdPIssuer: cfg.OIDC.Issuer,
		Log:       log,
	})
	if err != nil {
		return err
	}

	idTokenSigner, err := broker.NewIDTokenSigner([]byte(cfg.OIDC.BrokerSigningKey))
	if err != nil {
		return err
	}
	log.Info("memory broker: id_token signer ready", "kid", idTokenSigner.KeyID())

	srv := broker.NewServer(cfg, signer, verifier, store, discovery, idTokenSigner, log)
	if cfg.RateLimit.Disabled {
		log.Info("memory broker: rate limit disabled (rateLimit.disabled=true)")
	} else {
		log.Info("memory broker: rate limit enabled",
			"tokensPerMin", cfg.RateLimit.Tokens.PerMinute,
			"tokensBurst", cfg.RateLimit.Tokens.Burst,
			"loginPerMin", cfg.RateLimit.Login.PerMinute,
			"loginBurst", cfg.RateLimit.Login.Burst,
			"trustForwardedFor", cfg.RateLimit.TrustForwardedFor)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		log.Info("memory broker: listening", "addr", cfg.Server.Addr)
		err := httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	// MCP gateway on its own listener, memory profile: tenant-scoped
	// tools, soul seeding, soul prompts, endpoint instructions.
	mcpSrv := &http.Server{
		Addr:              cfg.Server.MCP.Addr,
		Handler:           srv.MemoryMCPGateway(version),
		ReadHeaderTimeout: 10 * time.Second,
	}
	mcpErrs := make(chan error, 1)
	mcpTLS := cfg.Server.MCP.TLS
	go func() {
		log.Info("memory broker: mcp gateway listening",
			"addr", cfg.Server.MCP.Addr, "tls", mcpTLS.CertFile != "")
		var err error
		if mcpTLS.CertFile != "" {
			err = mcpSrv.ListenAndServeTLS(mcpTLS.CertFile, mcpTLS.KeyFile)
		} else {
			err = mcpSrv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			mcpErrs <- err
			return
		}
		mcpErrs <- nil
	}()

	// Sweeper + device janitor lifecycles mirror the knowledge broker:
	// leader-elected sweeper across replicas, per-replica janitor, one
	// cancel tears both down before HTTP shutdown.
	sweepCtx, cancelSweep := context.WithCancel(context.Background())
	defer cancelSweep()
	var sweepWG sync.WaitGroup
	if !cfg.Sweeper.Disabled {
		sweeper := broker.NewSweeper(k8s, srv.RefreshStore(), cfg.Sweeper.Interval, log)
		if cfg.FileBackend() {
			log.Info("memory broker: starting sweeper (single-host)", "interval", cfg.Sweeper.Interval)
			sweepWG.Go(func() {
				sweeper.Run(sweepCtx)
			})
		} else {
			identity := brokerIdentity()
			log.Info("memory broker: starting sweeper",
				"interval", cfg.Sweeper.Interval, "leaseName", cfg.Sweeper.LeaseName,
				"namespace", cfg.Server.BrokerNamespace, "identity", identity)
			sweepWG.Go(func() {
				sweeper.RunLeaderElected(sweepCtx, cfg.Sweeper.LeaseName, cfg.Server.BrokerNamespace, identity)
			})
		}
	} else {
		log.Info("memory broker: sweeper disabled (sweeper.disabled=true)")
	}

	log.Info("memory broker: starting device-store janitor",
		"deviceCodeTTL", cfg.Server.DeviceCodeTTL,
		"devicePollInterval", cfg.Server.DevicePollInterval)
	sweepWG.Go(func() {
		srv.RunDeviceJanitor(sweepCtx)
	})

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Info("memory broker: received signal, shutting down", "signal", sig.String())
	case err := <-errs:
		if err != nil {
			cancelSweep()
			sweepWG.Wait()
			return err
		}
	case err := <-mcpErrs:
		if err != nil {
			cancelSweep()
			sweepWG.Wait()
			return err
		}
	}

	cancelSweep()
	sweepWG.Wait()

	// Separate shutdown contexts per surface; management API outcome is
	// the authoritative liveness signal (see the knowledge broker's main).
	mcpShutdownCtx, cancelMCPShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelMCPShutdown()
	if err := mcpSrv.Shutdown(mcpShutdownCtx); err != nil {
		log.Warn("memory broker: mcp gateway shutdown error", "err", err)
	}
	srv.CloseMCPGateway()
	mgmtShutdownCtx, cancelMgmtShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelMgmtShutdown()
	return httpSrv.Shutdown(mgmtShutdownCtx)
}

// brokerIdentity returns the holder identity stamped onto the leader-
// election Lease; POD_NAME via the downward API in-cluster, hostname or
// a literal fallback elsewhere.
func brokerIdentity() string {
	if v := os.Getenv("POD_NAME"); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "memory-broker"
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
