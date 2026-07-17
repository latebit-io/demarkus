// demarkus-broker is the OIDC token broker that exchanges a verified
// identity for one or more demarkus tokens, writing token hashes into
// per-world Kubernetes Secrets while tracking ownership in its own
// broker-namespace Secret.
//
// Current scope: single broker, multi-world support with
// domain/groups/emails allowlists (Slice C.1), browser code-flow for
// /auth/login + /auth/callback, leader-elected refresh-token sweeper
// (Slice C.2), per-subject + per-IP rate limiting (Slice C.4) via
// in-memory token buckets keyed by hashSubject(claims.Subject) on authed
// routes and source IP on /auth/login, RFC 8628 device flow +
// broker-signed refresh tokens + RFC 7009 revoke (universe-onboarding
// PRs 3-4), GET /me/install per-user install bundle (universe-onboarding
// PR5), and the always-on MCP-over-HTTPS gateway at /mcp. World access
// is mediated entirely through the gateway: SSO is the org gate, reads
// are open, and writes use a long-lived per-world token the broker
// provisions and holds. The broker no longer mints per-user tokens.
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

	"github.com/latebit-io/demarkus/tools/demarkus-broker/internal/broker"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/demarkus-broker/config.yaml", "path to broker YAML config")
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
		log.Error("broker exited with error", "err", err)
		os.Exit(1)
	}
}

func run(configPath, kubeconfigPath string, log *slog.Logger) error {
	cfg, err := broker.LoadConfig(configPath)
	if err != nil {
		return err
	}
	log.Info("broker: config loaded",
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
	// to start rather than failing the first user login. coreos/go-oidc
	// does not use this context for ongoing JWKS refresh (it builds its
	// own background context internally), so a plain Background suffices.
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
		log.Info("broker: file storage backend", "dir", cfg.Storage.Dir)
	} else {
		k8s, err = newKubeClient(kubeconfigPath)
		if err != nil {
			return err
		}
		store = broker.NewK8sSecretStore(k8s)
	}

	// Discovery does an eager IdP fetch at startup — same failure mode
	// as NewVerifier above (fails fast on misconfigured / unreachable
	// IdP). 5-minute TTL refresh keeps key-rotation propagation bounded
	// per plan §Risks.
	discovery, err := broker.NewDiscovery(context.Background(), broker.DiscoveryConfig{
		BrokerURL: cfg.Server.PublicURL,
		IdPIssuer: cfg.OIDC.Issuer,
		Log:       log,
	})
	if err != nil {
		return err
	}

	// Broker-side ECDSA signer for the /device/token refresh-grant
	// path (PR4). Required because PR4 ships broker-signed
	// id_tokens with a broker-hosted JWKS — without this, refresh
	// responses can't be verified by /me/install in PR5. Parsed
	// once at startup; an invalid PEM fails the pod fast rather
	// than the first refresh.
	idTokenSigner, err := broker.NewIDTokenSigner([]byte(cfg.OIDC.BrokerSigningKey))
	if err != nil {
		return err
	}
	log.Info("broker: id_token signer ready", "kid", idTokenSigner.KeyID())

	srv := broker.NewServer(cfg, signer, verifier, store, discovery, idTokenSigner, log)
	if cfg.RateLimit.Disabled {
		log.Info("broker: rate limit disabled (rateLimit.disabled=true)")
	} else {
		log.Info("broker: rate limit enabled",
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
		log.Info("broker: listening", "addr", cfg.Server.Addr)
		err := httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	// MCP gateway runs on its own listener alongside the management
	// API (plan: Broker MCP Gateway, Slice 1). Always on — every
	// broker is a knowledge-system gateway. Distinct Addr lets the
	// chart route the two surfaces through different Ingress hosts
	// or paths; shared auth + rate-limit + Issuer + Discovery
	// machinery means only the listener and the JSON-RPC transport
	// are new.
	mcpSrv := &http.Server{
		Addr:              cfg.Server.MCP.Addr,
		Handler:           srv.MCPGateway(version),
		ReadHeaderTimeout: 10 * time.Second,
	}
	mcpErrs := make(chan error, 1)
	mcpTLS := cfg.Server.MCP.TLS
	go func() {
		log.Info("broker: mcp gateway listening",
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

	// Sweeper runs alongside HTTP — leader election keeps it singleton
	// across replicas. Its lifecycle hangs off sweepCtx so the signal
	// path below can cancel it cleanly before HTTP shutdown begins.
	// ReleaseOnCancel inside RunLeaderElected gives the lease back so a
	// rolling restart's successor takes over in milliseconds.
	sweepCtx, cancelSweep := context.WithCancel(context.Background())
	defer cancelSweep()
	var sweepWG sync.WaitGroup
	if !cfg.Sweeper.Disabled {
		// Server owns the refresh store's lifecycle; the sweeper borrows
		// the same instance so both share one view of the grants.
		sweeper := broker.NewSweeper(k8s, srv.RefreshStore(), cfg.Sweeper.Interval, log)
		if cfg.FileBackend() {
			// Single-instance by construction: no Lease, no election.
			log.Info("broker: starting sweeper (single-host)", "interval", cfg.Sweeper.Interval)
			sweepWG.Go(func() {
				sweeper.Run(sweepCtx)
			})
		} else {
			identity := brokerIdentity()
			log.Info("broker: starting sweeper",
				"interval", cfg.Sweeper.Interval, "leaseName", cfg.Sweeper.LeaseName,
				"namespace", cfg.Server.BrokerNamespace, "identity", identity)
			sweepWG.Go(func() {
				sweeper.RunLeaderElected(sweepCtx, cfg.Sweeper.LeaseName, cfg.Server.BrokerNamespace, identity)
			})
		}
	} else {
		log.Info("broker: sweeper disabled (sweeper.disabled=true)")
	}

	// Device-store janitor evicts terminal RFC 8628 grants on a fixed
	// cadence. Per-replica state (single-broker invariant for now), no
	// leader election needed. Shares sweepCtx with the Sweeper so a
	// single cancel tears down both goroutines.
	log.Info("broker: starting device-store janitor",
		"deviceCodeTTL", cfg.Server.DeviceCodeTTL,
		"devicePollInterval", cfg.Server.DevicePollInterval)
	sweepWG.Go(func() {
		srv.RunDeviceJanitor(sweepCtx)
	})

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Info("broker: received signal, shutting down", "signal", sig.String())
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

	// Separate shutdown contexts so a slow MCP drain doesn't burn
	// the management-API deadline (and vice versa). Each surface
	// gets a fresh 10s; the management API's outcome is the
	// authoritative liveness signal, MCP shutdown errors are
	// logged best-effort.
	mcpShutdownCtx, cancelMCPShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelMCPShutdown()
	if err := mcpSrv.Shutdown(mcpShutdownCtx); err != nil {
		log.Warn("broker: mcp gateway shutdown error", "err", err)
	}
	// Drain the per-world fetch.Client pool's QUIC connections.
	// Done after http.Shutdown so any in-flight tool calls have
	// already returned (or timed out via their own contexts);
	// closing earlier could race against active streams.
	srv.CloseMCPGateway()
	mgmtShutdownCtx, cancelMgmtShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelMgmtShutdown()
	return httpSrv.Shutdown(mgmtShutdownCtx)
}

// brokerIdentity returns the holder identity stamped onto the leader-
// election Lease. In-cluster the operator wires POD_NAME via the
// downward API; out-of-cluster runs fall back to the host's hostname,
// or a literal "broker" string for unconfigured developer environments
// where Hostname() can return empty.
func brokerIdentity() string {
	if v := os.Getenv("POD_NAME"); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "broker"
}

// newKubeClient builds a kubernetes client. When kubeconfigPath is empty
// the in-cluster service-account config is used; otherwise the given
// kubeconfig (developer / out-of-cluster runs).
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
