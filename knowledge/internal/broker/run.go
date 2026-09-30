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
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/gateway"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/oauthsrv"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/storage"
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
	Profile *gateway.Profile
	// Validate runs extra config checks after LoadConfig.
	Validate []func(*core.Config) error
	// Buckets builds the tenant bucket backend; set by the memory broker
	// only. Provisioning is wired when it is set and the config enables it.
	// The cleanup runs once on every exit path.
	Buckets func(cfg *core.ProvisioningConfig, log *slog.Logger) (buckets storage.BucketCreator, cleanup func(), err error)
	// Version is the binary's build version (initialize response).
	Version string
	// KubeconfigPath selects an out-of-cluster kubeconfig; empty uses
	// the in-cluster service-account config.
	KubeconfigPath string
	// LocalWorlds, when set, serves the worlds it routes in process; the
	// world pool dials the rest.
	LocalWorlds gateway.LocalWorlds
}

// gatewayProfile is the product profile narrowed to the configured tool surface.
func (o *RunOptions) gatewayProfile(cfg *core.Config) *gateway.Profile {
	profile := *o.Profile
	profile.Tools = mcpfmt.ProfileTools(cfg.Server.MCP.ToolProfile, profile.Tools)
	return &profile
}

// Broker is an opened broker: config, auth machinery and both HTTP
// listeners built; Serve runs them, Close releases the rest.
type Broker struct {
	cfg         *core.Config
	opts        *RunOptions
	log         *slog.Logger
	pool        *gateway.WorldPool
	httpSrv     *http.Server
	mcpSrv      *http.Server
	tasks       *backgroundTasks
	closeBucket func()
	closeOnce   sync.Once
}

// Run is the shared broker main: Open, Serve until a shutdown signal,
// Close. The handler is registered before anything listens so a SIGTERM
// in the startup window still takes the graceful path.
func Run(configPath string, opts *RunOptions, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	b, err := Open(configPath, opts, log)
	if err != nil {
		return err
	}
	defer b.Close()
	err = b.Serve(ctx)
	if ctx.Err() != nil {
		log.Info(opts.LogName + ": received signal, shut down")
	}
	return err
}

// KnowledgeOptions is the knowledge broker's product surface.
func KnowledgeOptions(version, kubeconfigPath string) *RunOptions {
	return &RunOptions{
		LogName:        "broker",
		Realm:          "demarkus-knowledge-broker",
		Profile:        gateway.KnowledgeProfile(),
		Version:        version,
		KubeconfigPath: kubeconfigPath,
	}
}

// MemoryOptions is the memory broker's product surface: every static world
// names its tenant, the provisioning block is complete when enabled, and
// tenant buckets are GCS.
func MemoryOptions(version, kubeconfigPath string) *RunOptions {
	return &RunOptions{
		LogName: "memory broker",
		Realm:   "demarkus-memory-broker",
		Profile: gateway.MemoryProfile(),
		Validate: []func(*core.Config) error{
			(*core.Config).ValidateTenantWorlds,
			(*core.Config).ValidateProvisioning,
		},
		Buckets:        storage.NewGCSBuckets,
		Version:        version,
		KubeconfigPath: kubeconfigPath,
	}
}

// Open loads the config, builds the auth machinery, the OAuth server, the
// MCP gateway and both listeners. Nothing listens until Serve.
func Open(configPath string, opts *RunOptions, log *slog.Logger) (*Broker, error) {
	if opts.Profile == nil {
		return nil, errors.New("broker: RunOptions.Profile is required")
	}
	cfg, err := core.LoadConfig(configPath)
	if err != nil {
		return nil, err
	}
	for _, validate := range opts.Validate {
		if err := validate(cfg); err != nil {
			return nil, err
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
		return nil, err
	}
	deps, err := buildServerDeps(cfg, opts, store, log)
	if err != nil {
		return nil, err
	}
	srv := oauthsrv.NewServer(cfg, deps)
	provisioner, closeBuckets, err := enableProvisioning(cfg, opts, store, log)
	if err != nil {
		return nil, err
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

	// MCP gateway on its own listener; distinct Addr lets the chart
	// route the two surfaces through different Ingress hosts or paths
	// (SSE responses are write-side, untouched by the read timeouts).
	pool := gateway.NewWorldPool(cfg.Registry(), fetch.Options{Insecure: cfg.WorldDialer.InsecureSkipVerify})
	var dispatcher gateway.WorldDispatcher = pool
	if opts.LocalWorlds != nil {
		dispatcher = gateway.NewComposite(cfg.Registry(), opts.LocalWorlds, pool)
	}
	gw := gateway.New(gateway.DepsFor(cfg, deps.SharedDeps, store, provisioner), opts.Version, dispatcher, opts.gatewayProfile(cfg))
	return &Broker{
		cfg:         cfg,
		opts:        opts,
		log:         log,
		pool:        pool,
		httpSrv:     newHardenedServer(cfg.Server.Addr, srv.Routes()),
		mcpSrv:      newHardenedServer(cfg.Server.MCP.Addr, gw.Routes()),
		tasks:       &backgroundTasks{cfg: cfg, opts: opts, log: log, store: store, k8s: k8s, srv: srv, provisioner: provisioner},
		closeBucket: closeBuckets,
	}, nil
}

// Serve runs both listeners and the background tasks until ctx ends or a
// listener fails, then drains MCP, the world pool and last the management
// API, the liveness signal. Close still has to run afterwards.
func (b *Broker) Serve(ctx context.Context) error {
	cfg, opts, log := b.cfg, b.opts, b.log
	errs := make(chan error, 1)
	go func() {
		log.Info(opts.LogName+": listening", "addr", cfg.Server.Addr)
		errs <- filterServerClosed(b.httpSrv.ListenAndServe())
	}()
	mcpErrs := make(chan error, 1)
	mcpTLS := cfg.Server.MCP.TLS
	go func() {
		log.Info(opts.LogName+": mcp gateway listening",
			"addr", cfg.Server.MCP.Addr, "tls", mcpTLS.CertFile != "")
		if mcpTLS.CertFile != "" {
			mcpErrs <- filterServerClosed(b.mcpSrv.ListenAndServeTLS(mcpTLS.CertFile, mcpTLS.KeyFile))
			return
		}
		mcpErrs <- filterServerClosed(b.mcpSrv.ListenAndServe())
	}()

	// One cancel tears down every background task before HTTP shutdown.
	sweepCtx, cancelSweep := context.WithCancel(context.Background())
	defer cancelSweep()
	var sweepWG sync.WaitGroup
	b.tasks.start(sweepCtx, &sweepWG)

	select {
	case <-ctx.Done():
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
	if err := b.mcpSrv.Shutdown(mcpShutdownCtx); err != nil {
		log.Warn(opts.LogName+": mcp gateway shutdown error", "err", err)
	}
	// Drain pooled QUIC connections after http.Shutdown so in-flight
	// tool calls have already returned.
	b.pool.Close()
	mgmtShutdownCtx, cancelMgmtShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelMgmtShutdown()
	return b.httpSrv.Shutdown(mgmtShutdownCtx)
}

// Close releases the tenant bucket backend and the world pool; safe after
// Serve and more than once.
func (b *Broker) Close() {
	b.closeOnce.Do(func() {
		b.pool.Close()
		b.closeBucket()
	})
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

// backgroundTasks are the per-replica loops that run until shutdown.
type backgroundTasks struct {
	cfg         *core.Config
	opts        *RunOptions
	log         *slog.Logger
	store       core.SecretStore
	k8s         kubernetes.Interface
	srv         *oauthsrv.Server
	provisioner *storage.Provisioner
}

// start launches the refresh token sweeper (leader-elected unless single-host),
// the device janitor, the agent token reconciler when agentTokens is set, and
// the registry sync when provisioning is on.
func (b *backgroundTasks) start(ctx context.Context, wg *sync.WaitGroup) {
	b.startSweeper(ctx, wg)
	b.log.Info(b.opts.LogName+": starting device-store janitor",
		"deviceCodeTTL", b.cfg.Server.DeviceCodeTTL,
		"devicePollInterval", b.cfg.Server.DevicePollInterval)
	wg.Go(func() {
		b.srv.RunDeviceJanitor(ctx)
	})
	if len(b.cfg.AgentTokens) > 0 {
		agentTokens := storage.NewAgentTokens(b.cfg, b.store, b.log)
		b.log.Info(b.opts.LogName+": starting agent token reconciler", "worlds", len(b.cfg.AgentTokens))
		wg.Go(func() {
			agentTokens.Run(ctx)
		})
	}
	if b.provisioner != nil {
		// Keeps this replica converged with tenants its siblings provisioned.
		wg.Go(func() {
			b.provisioner.RunRegistrySync(ctx)
		})
	}
}

func (b *backgroundTasks) startSweeper(ctx context.Context, wg *sync.WaitGroup) {
	cfg := b.cfg
	if cfg.Sweeper.Disabled {
		b.log.Info(b.opts.LogName + ": sweeper disabled (sweeper.disabled=true)")
		return
	}
	sweeper := storage.NewSweeper(b.k8s, b.srv.RefreshStore(), cfg.Sweeper.Interval, b.log)
	if cfg.FileBackend() {
		b.log.Info(b.opts.LogName+": starting sweeper (single-host)", "interval", cfg.Sweeper.Interval)
		wg.Go(func() {
			sweeper.Run(ctx)
		})
		return
	}
	identity := brokerIdentity()
	b.log.Info(b.opts.LogName+": starting sweeper",
		"interval", cfg.Sweeper.Interval, "leaseName", cfg.Sweeper.LeaseName,
		"namespace", cfg.Server.BrokerNamespace, "identity", identity)
	wg.Go(func() {
		sweeper.RunLeaderElected(ctx, cfg.Sweeper.LeaseName, cfg.Server.BrokerNamespace, identity)
	})
}

// stateCookieKey is server.cookieKey when configured, else the key persisted
// in the store and generated on first start.
func stateCookieKey(cfg *core.Config, opts *RunOptions, store core.SecretStore, log *slog.Logger) (string, error) {
	if cfg.Server.CookieKey != "" {
		return cfg.Server.CookieKey, nil
	}
	ref := core.CookieKeyRef(cfg)
	key, err := core.EnsureCookieKey(context.Background(), store, ref)
	if err != nil {
		return "", err
	}
	log.Info(opts.LogName+": state cookie key from store", "ref", ref.String(), "generated", key.Generated)
	return key.Key, nil
}

// buildServerDeps constructs the auth machinery both listeners share:
// signer, verifier, discovery, the broker-side id_token signer and the rate
// limiters, each failing fast on misconfiguration.
func buildServerDeps(cfg *core.Config, opts *RunOptions, store core.SecretStore, log *slog.Logger) (oauthsrv.ServerDeps, error) {
	cookieKey, err := stateCookieKey(cfg, opts, store, log)
	if err != nil {
		return oauthsrv.ServerDeps{}, err
	}
	signer, err := oauthsrv.NewSigner(cookieKey)
	if err != nil {
		return oauthsrv.ServerDeps{}, err
	}

	// OIDC discovery is eager so a misconfigured broker fails to start
	// rather than failing the first user login. coreos/go-oidc builds
	// its own background context for JWKS refresh, so Background is fine.
	verifier, err := core.NewVerifier(context.Background(), &cfg.OIDC)
	if err != nil {
		return oauthsrv.ServerDeps{}, err
	}
	if cfg.FileBackend() {
		log.Info(opts.LogName+": file storage backend", "dir", cfg.Storage.Dir)
	}

	// Same eager-failure posture as NewVerifier; 5-minute TTL bounds
	// IdP key-rotation propagation.
	discovery, err := oauthsrv.NewDiscovery(context.Background(), oauthsrv.DiscoveryConfig{
		BrokerURL: cfg.Server.PublicURL,
		IdPIssuer: cfg.OIDC.Issuer,
		Log:       log,
	})
	if err != nil {
		return oauthsrv.ServerDeps{}, err
	}

	// Broker-side ECDSA signer for the refresh-grant path; an invalid
	// PEM fails the pod fast rather than the first refresh. No configured
	// key means one is generated and persisted on first start.
	var idTokenSigner *core.IDTokenSigner
	if cfg.OIDC.BrokerSigningKey != "" {
		if idTokenSigner, err = core.NewIDTokenSigner([]byte(cfg.OIDC.BrokerSigningKey)); err != nil {
			return oauthsrv.ServerDeps{}, err
		}
	} else {
		ref := core.SigningKeyRef(cfg)
		key, err := core.EnsureSigningKey(context.Background(), store, ref)
		if err != nil {
			return oauthsrv.ServerDeps{}, err
		}
		idTokenSigner = key.Signer
		log.Info(opts.LogName+": signing key from store", "ref", ref.String(), "generated", key.Generated)
	}
	log.Info(opts.LogName+": id_token signer ready", "kid", idTokenSigner.KeyID())

	subject, login := core.NewRateLimits(&cfg.RateLimit)
	return oauthsrv.ServerDeps{
		SharedDeps: core.SharedDeps{
			Verifier:       core.VerifierWith(verifier, idTokenSigner, cfg.Server.PublicURL),
			SubjectLimiter: subject,
			Clock:          time.Now,
			Log:            log,
		},
		Signer:        signer,
		Store:         store,
		Discovery:     discovery,
		IDTokenSigner: idTokenSigner,
		LoginLimiter:  login,
		TenantScoped:  opts.Profile.TenantScoped,
	}, nil
}

// enableProvisioning builds the tenant provisioner when the binary supplies a
// bucket backend and the config enables provisioning; nil otherwise. The
// cleanup releases the bucket client and is safe to call either way.
func enableProvisioning(cfg *core.Config, opts *RunOptions, store core.SecretStore, log *slog.Logger) (*storage.Provisioner, func(), error) {
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
	return storage.NewProvisioner(cfg, storage.ProvisionerDeps{Store: store, Buckets: buckets, Log: log}), closeBuckets, nil
}

// newSecretStore selects the credential backend from config: file mode
// for single-host, otherwise Secrets via a kubernetes client.
func newSecretStore(cfg *core.Config, kubeconfigPath string) (core.SecretStore, kubernetes.Interface, error) {
	if cfg.FileBackend() {
		return storage.NewFileSecretStore(), nil, nil
	}
	k8s, err := newKubeClient(kubeconfigPath)
	if err != nil {
		return nil, nil, err
	}
	return storage.NewK8sSecretStore(k8s), k8s, nil
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
	cfg, err := core.LoadConfig(opts.ConfigPath)
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
	var buckets storage.BucketCreator
	if opts.DeleteBucket {
		gcsBuckets, cleanup, bucketsErr := storage.NewGCSBuckets(&cfg.Provisioning, opts.Log)
		if bucketsErr != nil {
			return false, bucketsErr
		}
		defer cleanup()
		buckets = gcsBuckets
	}
	return storage.NewProvisioner(cfg, storage.ProvisionerDeps{Store: store, Buckets: buckets, Log: opts.Log}).DeprovisionTenant(ctx, opts.Slug, opts.DeleteBucket)
}
