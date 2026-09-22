package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/mcpfmt"
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
	// Buckets builds the tenant bucket backend; set by the memory broker
	// only. Provisioning is wired when it is set and the config enables it.
	// The cleanup runs once on every exit path.
	Buckets func(cfg *ProvisioningConfig, log *slog.Logger) (buckets BucketCreator, cleanup func(), err error)
	// Version is the binary's build version (initialize response).
	Version string
	// KubeconfigPath selects an out-of-cluster kubeconfig; empty uses
	// the in-cluster service-account config.
	KubeconfigPath string
}

// gatewayProfile is the product profile narrowed to the configured tool surface.
func (o *RunOptions) gatewayProfile(cfg *Config) *GatewayProfile {
	profile := *o.Profile
	profile.Tools = mcpfmt.ProfileTools(cfg.Server.MCP.ToolProfile, profile.Tools)
	return &profile
}

// Run is the shared broker main: config, auth machinery, both HTTP
// listeners, sweeper + device janitor, signal handling, and the
// two-phase shutdown. Blocks until shutdown completes.
func Run(configPath string, opts *RunOptions, log *slog.Logger) error {
	if opts.Profile == nil {
		return errors.New("broker: RunOptions.Profile is required")
	}
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

	// Storage backend: kubernetes needs a client; file mode (single-host)
	// runs with no cluster at all.
	store, k8s, err := newSecretStore(cfg, opts.KubeconfigPath)
	if err != nil {
		return err
	}
	deps, err := buildServerDeps(cfg, opts, store, log)
	if err != nil {
		return err
	}
	srv := NewServer(cfg, deps)
	provisioner, closeBuckets, err := enableProvisioning(cfg, opts, store, log)
	if err != nil {
		return err
	}
	defer closeBuckets()

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

	httpSrv := newHardenedServer(cfg.Server.Addr, srv.Routes())
	errs := make(chan error, 1)
	go func() {
		log.Info(opts.LogName+": listening", "addr", cfg.Server.Addr)
		errs <- filterServerClosed(httpSrv.ListenAndServe())
	}()

	// MCP gateway on its own listener; distinct Addr lets the chart
	// route the two surfaces through different Ingress hosts or paths
	// (SSE responses are write-side, untouched by the read timeouts).
	pool := newWorldPool(cfg.worlds(), fetch.Options{Insecure: cfg.WorldDialer.InsecureSkipVerify})
	gateway := newMCPGateway(gatewayDepsFor(cfg, &deps, store, provisioner), opts.Version, pool, opts.gatewayProfile(cfg))
	mcpSrv := newHardenedServer(cfg.Server.MCP.Addr, gateway.Routes())
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
	if provisioner != nil {
		// The registry sync keeps this replica converged with tenants
		// provisioned by its siblings.
		sweepWG.Go(func() {
			provisioner.RunRegistrySync(sweepCtx)
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
	pool.Close()
	mgmtShutdownCtx, cancelMgmtShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelMgmtShutdown()
	return httpSrv.Shutdown(mgmtShutdownCtx)
}

// newHardenedServer is the one timeout policy for every broker listener:
// header 10s, whole-request read 60s, keep-alive idle 120s.
func newHardenedServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
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

// buildServerDeps constructs the auth machinery both listeners share:
// signer, verifier, discovery, the broker-side id_token signer and the rate
// limiters, each failing fast on misconfiguration.
func buildServerDeps(cfg *Config, opts *RunOptions, store SecretStore, log *slog.Logger) (ServerDeps, error) {
	signer, err := NewSigner(cfg.Server.CookieKey)
	if err != nil {
		return ServerDeps{}, err
	}

	// OIDC discovery is eager so a misconfigured broker fails to start
	// rather than failing the first user login. coreos/go-oidc builds
	// its own background context for JWKS refresh, so Background is fine.
	verifier, err := NewVerifier(context.Background(), &cfg.OIDC)
	if err != nil {
		return ServerDeps{}, err
	}
	if cfg.FileBackend() {
		log.Info(opts.LogName+": file storage backend", "dir", cfg.Storage.Dir)
	}

	// Same eager-failure posture as NewVerifier; 5-minute TTL bounds
	// IdP key-rotation propagation.
	discovery, err := NewDiscovery(context.Background(), DiscoveryConfig{
		BrokerURL: cfg.Server.PublicURL,
		IdPIssuer: cfg.OIDC.Issuer,
		Log:       log,
	})
	if err != nil {
		return ServerDeps{}, err
	}

	// Broker-side ECDSA signer for the refresh-grant path; an invalid
	// PEM fails the pod fast rather than the first refresh.
	idTokenSigner, err := NewIDTokenSigner([]byte(cfg.OIDC.BrokerSigningKey))
	if err != nil {
		return ServerDeps{}, err
	}
	log.Info(opts.LogName+": id_token signer ready", "kid", idTokenSigner.KeyID())

	subject, login := newRateLimits(&cfg.RateLimit)
	return ServerDeps{
		Signer:         signer,
		Verifier:       verifierWith(verifier, idTokenSigner, cfg.Server.PublicURL),
		Store:          store,
		Discovery:      discovery,
		IDTokenSigner:  idTokenSigner,
		Log:            log,
		Clock:          time.Now,
		SubjectLimiter: subject, LoginLimiter: login,
		TenantScoped: opts.Profile.TenantScoped,
	}, nil
}

// enableProvisioning builds the tenant provisioner when the binary supplies a
// bucket backend and the config enables provisioning; nil otherwise. The
// cleanup releases the bucket client and is safe to call either way.
func enableProvisioning(cfg *Config, opts *RunOptions, store SecretStore, log *slog.Logger) (*Provisioner, func(), error) {
	if opts.Buckets == nil || !cfg.Provisioning.Enabled() {
		return nil, func() {}, nil
	}
	buckets, closeBuckets, err := opts.Buckets(&cfg.Provisioning, log)
	if err != nil {
		return nil, nil, fmt.Errorf("provisioning enabled: %w", err)
	}
	log.Info(opts.LogName+": provisioning enabled",
		"mode", cfg.Provisioning.Mode,
		"maxTenants", cfg.Provisioning.MaxTenants,
		"authorityDomain", cfg.Provisioning.AuthorityDomain)
	return newProvisioner(cfg, provisionerDeps{Store: store, Buckets: buckets, Log: log}), closeBuckets, nil
}

// gatewayDepsFor shares the verifier, clock, log and subject limiter with the
// management API and gives the gateway its own write token store over the
// shared Secret store.
func gatewayDepsFor(cfg *Config, srv *ServerDeps, store SecretStore, provisioner *Provisioner) *gatewayDeps {
	return &gatewayDeps{
		Worlds:         cfg.worlds(),
		Issuer:         cfg.OIDC.Issuer,
		PublicURL:      cfg.Server.PublicURL,
		MCP:            cfg.Server.MCP,
		Realm:          cfg.Server.Realm,
		Verifier:       srv.Verifier,
		AllowDomains:   cfg.OIDC.AllowDomains,
		SubjectLimiter: srv.SubjectLimiter,
		Clock:          srv.Clock,
		Log:            srv.Log,
		WriteTokens:    newWorldWriteTokenStore(cfg, store),
		Provisioner:    provisioner,
	}
}

// newSecretStore selects the credential backend from config: file mode
// for single-host, otherwise Secrets via a kubernetes client.
func newSecretStore(cfg *Config, kubeconfigPath string) (SecretStore, kubernetes.Interface, error) {
	if cfg.FileBackend() {
		return NewFileSecretStore(), nil, nil
	}
	k8s, err := newKubeClient(kubeconfigPath)
	if err != nil {
		return nil, nil, err
	}
	return NewK8sSecretStore(k8s), k8s, nil
}

// DeprovisionOptions names one tenant world to remove.
type DeprovisionOptions struct {
	ConfigPath, KubeconfigPath string
	Slug                       string
	DeleteBucket               bool
	Log                        *slog.Logger
}

// deprovisionTimeout bounds one run. Emptying a large bucket is slow, and a
// run that is cut short converges when it is run again.
const deprovisionTimeout = 15 * time.Minute

// RunDeprovision is the operator deprovision flow, owned here so its wiring
// (validators, store, buckets) can never drift from Run's. It stops on
// SIGINT or SIGTERM, leaving the tombstone for the rerun to finish.
func RunDeprovision(ctx context.Context, opts DeprovisionOptions) (found bool, err error) {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, deprovisionTimeout)
	defer cancel()
	cfg, err := LoadConfig(opts.ConfigPath)
	if err != nil {
		return false, err
	}
	if err := cfg.ValidateTenantWorlds(); err != nil {
		return false, err
	}
	if err := cfg.ValidateProvisioning(); err != nil {
		return false, err
	}
	if !cfg.Provisioning.Enabled() {
		return false, fmt.Errorf("deprovisioning requires provisioning enabled (mode %q)", cfg.Provisioning.Mode)
	}
	store, _, err := newSecretStore(cfg, opts.KubeconfigPath)
	if err != nil {
		return false, err
	}
	// Secret-only cleanup must not depend on GCS reachability.
	var buckets BucketCreator
	if opts.DeleteBucket {
		gcsBuckets, cleanup, bucketsErr := NewGCSBuckets(&cfg.Provisioning, opts.Log)
		if bucketsErr != nil {
			return false, bucketsErr
		}
		defer cleanup()
		buckets = gcsBuckets
	}
	return newProvisioner(cfg, provisionerDeps{Store: store, Buckets: buckets, Log: opts.Log}).DeprovisionTenant(ctx, opts.Slug, opts.DeleteBucket)
}
