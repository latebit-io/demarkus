// demarkus-broker is the OIDC token broker that exchanges a verified
// identity for one or more demarkus tokens, writing token hashes into
// per-world Kubernetes Secrets while tracking ownership in its own
// broker-namespace Secret.
//
// Current scope: single broker, multi-world support with
// domain/groups/emails allowlists (Slice C.1), browser code-flow for
// /auth/login + /auth/callback, bearer-token (ID token) authentication
// for /tokens, DELETE /tokens/:label, and POST /tokens/:label/rotate
// (Slice C.3), leader-elected expiry/drift sweeper (Slice C.2), and
// per-subject + per-IP rate limiting (Slice C.4) via in-memory token
// buckets keyed by hashSubject(claims.Subject) on authed routes and
// source IP on /auth/login.
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

	"github.com/latebit/demarkus/tools/demarkus-broker/internal/broker"
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
	verifier, err := broker.NewVerifier(context.Background(), cfg.OIDC)
	if err != nil {
		return err
	}

	k8s, err := newKubeClient(kubeconfigPath)
	if err != nil {
		return err
	}

	issuer := broker.NewIssuer(cfg, k8s)

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

	srv := broker.NewServer(cfg, signer, verifier, issuer, discovery, log)
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

	// Sweeper runs alongside HTTP — leader election keeps it singleton
	// across replicas. Its lifecycle hangs off sweepCtx so the signal
	// path below can cancel it cleanly before HTTP shutdown begins.
	// ReleaseOnCancel inside RunLeaderElected gives the lease back so a
	// rolling restart's successor takes over in milliseconds.
	sweepCtx, cancelSweep := context.WithCancel(context.Background())
	defer cancelSweep()
	var sweepWG sync.WaitGroup
	if !cfg.Sweeper.Disabled {
		sweeper := broker.NewSweeper(issuer, cfg.Sweeper.Interval, log)
		identity := brokerIdentity()
		log.Info("broker: starting sweeper",
			"interval", cfg.Sweeper.Interval, "leaseName", cfg.Sweeper.LeaseName,
			"namespace", cfg.Server.BrokerNamespace, "identity", identity)
		sweepWG.Go(func() {
			sweeper.RunLeaderElected(sweepCtx, cfg.Sweeper.LeaseName, cfg.Server.BrokerNamespace, identity)
		})
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
	}

	cancelSweep()
	sweepWG.Wait()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	return httpSrv.Shutdown(shutdownCtx)
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
