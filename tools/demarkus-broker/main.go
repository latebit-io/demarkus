// demarkus-broker is the OIDC token broker that exchanges a verified
// identity for one or more demarkus tokens, writing token hashes into
// per-world Kubernetes Secrets while tracking ownership in its own
// broker-namespace Secret.
//
// Slice B scope (this binary): single broker, multi-world support behind
// per-world domain allowlists, browser code-flow for /auth/login +
// /auth/callback, bearer-token (ID token) authentication for /tokens and
// DELETE /tokens/:label. No expiry sweeper, no leader election, no
// rotate, no rate limit — those land in Slice C/D.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	srv := broker.NewServer(cfg, signer, verifier, issuer, log)

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

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Info("broker: received signal, shutting down", "signal", sig.String())
	case err := <-errs:
		if err != nil {
			return err
		}
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	return httpSrv.Shutdown(shutdownCtx)
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
