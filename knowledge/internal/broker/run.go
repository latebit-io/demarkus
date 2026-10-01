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

	gcs "cloud.google.com/go/storage"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/gateway"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/oauthsrv"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/storage"
	"github.com/latebit-io/demarkus/protocol"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// RunOptions parameterizes the broker lifecycle for the binary that hosts it.
type RunOptions struct {
	// Version is the binary's build version (initialize response).
	Version string
	// KubeconfigPath selects an out-of-cluster kubeconfig; empty uses
	// the in-cluster service-account config.
	KubeconfigPath string
	// LocalWorlds serves the worlds marked local in process and takes
	// provisioned tenants directly; the world pool dials the rest. Every
	// local world must be routed by it at Open.
	LocalWorlds LocalServer
}

// LocalServer is the knowledge server the composed binary hosts: it
// dispatches its worlds in process, applies the provisioner's worlds and
// opens the bearer listener the broker's gate admits requests on.
type LocalServer interface {
	gateway.LocalWorlds
	storage.WorldsApplier
	OpenBearerListener(address string, gates func(authority string) (protocol.Gate, error)) error
}

// Broker is an opened broker: config, auth machinery and the listeners
// built; Serve runs them, Close releases the rest.
type Broker struct {
	cfg  *core.Config
	log  *slog.Logger
	pool *gateway.WorldPool
	srv  *oauthsrv.Server
	// httpSrv is the one listener: management API and both gateways.
	httpSrv     *http.Server
	gateways    []*gateway.Gateway
	tasks       *backgroundTasks
	closeBucket func()
	closeOnce   sync.Once
}

// Options is the broker's product surface: both gateway profiles from one
// config, tenant buckets on GCS when provisioning is enabled.
func Options(version, kubeconfigPath string) *RunOptions {
	return &RunOptions{
		Version:        version,
		KubeconfigPath: kubeconfigPath,
	}
}

// Open loads the config, builds the auth machinery, the OAuth server, the
// gateways and the listeners. Nothing listens until Serve.
func Open(configPath string, opts *RunOptions, log *slog.Logger) (*Broker, error) {
	cfg, err := core.LoadConfig(configPath)
	if err != nil {
		return nil, err
	}
	log.Info("broker: config loaded",
		"addr", cfg.Server.Addr,
		"oidcIssuer", cfg.OIDC.Issuer,
		"worlds", len(cfg.Worlds),
		"memoryGateway", cfg.Server.Memory.Enabled(),
		"version", opts.Version,
	)

	store, k8s, err := newSecretStore(opts.KubeconfigPath)
	if err != nil {
		return nil, err
	}
	deps, err := buildServerDeps(cfg, store, log)
	if err != nil {
		return nil, err
	}
	gcsClient, closeBuckets, err := storage.NewGCSClient(log)
	if err != nil {
		return nil, err
	}
	if deps.State, err = storage.OpenStateBucket(gcsClient, &cfg.Server, log); err != nil {
		closeBuckets()
		return nil, err
	}
	srv := oauthsrv.NewServer(cfg, deps)
	if err := importClientRegistrations(cfg, srv, store, log); err != nil {
		closeBuckets()
		return nil, err
	}
	provisioner, err := enableProvisioning(cfg, storage.ProvisionerDeps{Store: store, Worlds: opts.LocalWorlds, Log: log}, gcsClient)
	if err != nil {
		closeBuckets()
		return nil, err
	}

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

	if err := gateway.CheckLocal(cfg.Registry(), opts.LocalWorlds); err != nil {
		closeBuckets()
		return nil, err
	}
	if err := openBearerListener(cfg, opts.LocalWorlds, deps.Verifier, log); err != nil {
		closeBuckets()
		return nil, err
	}
	pool := gateway.NewWorldPool(cfg.Registry(), fetch.Options{Insecure: cfg.WorldDialer.InsecureSkipVerify})
	var dispatcher gateway.WorldDispatcher = pool
	if opts.LocalWorlds != nil {
		dispatcher = gateway.NewComposite(cfg.Registry(), opts.LocalWorlds, pool)
	}
	// One mux: the management API, the knowledge gateway on every host it
	// does not claim, the memory gateway on its own hostname.
	mux := http.NewServeMux()
	srv.Register(mux)
	knowledge := gateway.KnowledgeProfile()
	gateways := []*gateway.Gateway{gateway.New(gateway.DepsFor(cfg, knowledge, deps.SharedDeps, nil), opts.Version, dispatcher, knowledge)}
	gateways[0].Register(mux, "")
	if cfg.Server.Memory.Enabled() {
		memory := gateway.MemoryProfile()
		gateways = append(gateways, gateway.New(gateway.DepsFor(cfg, memory, deps.SharedDeps, provisioner), opts.Version, dispatcher, memory))
		gateways[1].Register(mux, cfg.Server.Memory.Host())
	}
	b := &Broker{
		cfg:         cfg,
		log:         log,
		pool:        pool,
		srv:         srv,
		httpSrv:     newHardenedServer(cfg.Server.Addr, mux),
		gateways:    gateways,
		tasks:       &backgroundTasks{cfg: cfg, log: log, store: store, k8s: k8s, srv: srv, provisioner: provisioner},
		closeBucket: closeBuckets,
	}
	return b, nil
}

// Serve runs the listeners and the background tasks until ctx ends or a
// listener fails, then flips readiness, drains the mux and the world pool.
// Close still has to run afterwards.
func (b *Broker) Serve(ctx context.Context) error {
	cfg, log := b.cfg, b.log
	errs := make(chan error, 1)
	go func() {
		log.Info("broker: listening", "addr", cfg.Server.Addr)
		errs <- filterServerClosed(b.httpSrv.ListenAndServe())
	}()

	// One cancel tears down every background task before HTTP shutdown.
	sweepCtx, cancelSweep := context.WithCancel(context.Background())
	defer cancelSweep()
	var sweepWG sync.WaitGroup
	b.tasks.start(sweepCtx, &sweepWG)

	// A failed listener still takes the common path, so the tasks stop
	// before the caller's Close releases what they serve from.
	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errs:
		if err != nil {
			runErr = fmt.Errorf("listener: %w", err)
		} else {
			log.Warn("broker: listener stopped, shutting down")
		}
	}

	// Readiness drops first so the load balancer stops sending sessions;
	// the tasks stop before the listeners so nothing writes into a drain.
	b.srv.BeginDrain()
	cancelSweep()
	sweepWG.Wait()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	shutdownErr := b.httpSrv.Shutdown(shutdownCtx)
	// Drain pooled QUIC connections after http.Shutdown so in-flight
	// tool calls have already returned.
	b.pool.Close()
	return errors.Join(runErr, shutdownErr)
}

// Close stops the listener outright, then the gateways' session sweeps,
// the world pool and the bucket clients; safe after Serve and more
// than once.
func (b *Broker) Close() {
	b.closeOnce.Do(func() {
		if err := filterServerClosed(b.httpSrv.Close()); err != nil {
			b.log.Warn("broker: listener close error", "addr", b.httpSrv.Addr, "err", err)
		}
		for _, g := range b.gateways {
			if err := g.Shutdown(context.Background()); err != nil {
				b.log.Warn("broker: gateway shutdown error", "err", err)
			}
		}
		b.pool.Close()
		b.closeBucket()
	})
}

// importClientRegistrations moves registrations from the pre-bucket Secret
// into the state bucket, so MCP hosts keep their client_id across the upgrade.
func importClientRegistrations(cfg *core.Config, srv *oauthsrv.Server, store core.SecretStore, log *slog.Logger) error {
	ref := core.DynamicClientsRef(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	imported, err := srv.DynamicClients().ImportSecret(ctx, store, ref)
	if err != nil {
		return fmt.Errorf("import client registrations from %s: %w", ref, err)
	}
	if imported > 0 {
		log.Info("broker: imported client registrations into the state bucket", "count", imported, "from", ref.String())
	}
	return nil
}

// openBearerListener has the server in this process listen for identity
// bearers on server.bearerAddr, admitted by the broker's gate.
func openBearerListener(cfg *core.Config, local LocalServer, verifier core.Verifier, log *slog.Logger) error {
	if cfg.Server.BearerAddr == "" {
		return nil
	}
	if local == nil {
		return errors.New("server.bearerAddr needs a knowledge server in this process")
	}
	return local.OpenBearerListener(cfg.Server.BearerAddr, core.NewBearerGate(cfg, verifier, log).For)
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
	log         *slog.Logger
	store       core.SecretStore
	k8s         kubernetes.Interface
	srv         *oauthsrv.Server
	provisioner *storage.Provisioner
}

// start launches the leader-elected state bucket sweeper, the agent token
// reconciler when agentTokens is set, and the registry sync when
// provisioning is on. In-flight grants sweep themselves on every write.
func (b *backgroundTasks) start(ctx context.Context, wg *sync.WaitGroup) {
	b.startSweeper(ctx, wg)
	if len(b.cfg.AgentTokens) > 0 {
		agentTokens := storage.NewAgentTokens(b.cfg, b.store, b.log)
		b.log.Info("broker: starting agent token reconciler", "worlds", len(b.cfg.AgentTokens))
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
		b.log.Info("broker: sweeper disabled (sweeper.disabled=true)")
		return
	}
	sweeper := storage.NewSweeper(b.k8s, []storage.SweptStore{
		{Name: "refresh tokens", Store: b.srv.RefreshStore()},
		{Name: "client registrations", Store: b.srv.DynamicClients()},
	}, cfg.Sweeper.Interval, b.log)
	identity := brokerIdentity()
	b.log.Info("broker: starting sweeper",
		"interval", cfg.Sweeper.Interval, "leaseName", cfg.Sweeper.LeaseName,
		"namespace", cfg.Server.BrokerNamespace, "identity", identity)
	wg.Go(func() {
		sweeper.RunLeaderElected(ctx, cfg.Sweeper.LeaseName, cfg.Server.BrokerNamespace, identity)
	})
}

// stateCookieKey is server.cookieKey when configured, else the key persisted
// in the store and generated on first start.
func stateCookieKey(cfg *core.Config, store core.SecretStore, log *slog.Logger) (string, error) {
	if cfg.Server.CookieKey != "" {
		return cfg.Server.CookieKey, nil
	}
	ref := core.CookieKeyRef(cfg)
	key, err := core.EnsureCookieKey(context.Background(), store, ref)
	if err != nil {
		return "", err
	}
	log.Info("broker: state cookie key from store", "ref", ref.String(), "generated", key.Generated)
	return key.Key, nil
}

// buildServerDeps constructs the auth machinery both listeners share:
// signer, verifier, discovery, the broker-side id_token signer and the rate
// limiters, each failing fast on misconfiguration.
func buildServerDeps(cfg *core.Config, store core.SecretStore, log *slog.Logger) (oauthsrv.ServerDeps, error) {
	cookieKey, err := stateCookieKey(cfg, store, log)
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
		log.Info("broker: signing key from store", "ref", ref.String(), "generated", key.Generated)
	}
	log.Info("broker: id_token signer ready", "kid", idTokenSigner.KeyID())

	subject, login := core.NewRateLimits(&cfg.RateLimit)
	return oauthsrv.ServerDeps{
		SharedDeps: core.SharedDeps{
			Verifier:       core.VerifierWith(verifier, idTokenSigner, cfg.Server.PublicURL, cfg.Server.Resources()),
			SubjectLimiter: subject,
			Clock:          time.Now,
			Log:            log,
		},
		Signer:        signer,
		Store:         store,
		Discovery:     discovery,
		IDTokenSigner: idTokenSigner,
		LoginLimiter:  login,
	}, nil
}

// enableProvisioning builds the tenant provisioner over gcsClient when the
// config enables provisioning; nil otherwise. deps arrives without Buckets.
func enableProvisioning(cfg *core.Config, deps storage.ProvisionerDeps, gcsClient *gcs.Client) (*storage.Provisioner, error) {
	if !cfg.Provisioning.Enabled() {
		return nil, nil
	}
	buckets, err := storage.NewGCSBuckets(gcsClient, &cfg.Provisioning, deps.Log)
	if err != nil {
		return nil, fmt.Errorf("provisioning enabled: %w", err)
	}
	deps.Log.Info("broker: provisioning enabled",
		"mode", cfg.Provisioning.Mode,
		"maxTenants", cfg.Provisioning.MaxTenants,
		"authorityDomain", cfg.Provisioning.AuthorityDomain)
	deps.Buckets = buckets
	return storage.NewProvisioner(cfg, deps), nil
}

// newSecretStore is the one place Open and RunDeprovision build the
// Secret store from, so their wiring cannot drift.
func newSecretStore(kubeconfigPath string) (core.SecretStore, kubernetes.Interface, error) {
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
// (validators, store, buckets) can never drift from Open's. It stops on
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
	if !cfg.Provisioning.Enabled() {
		return false, fmt.Errorf("deprovisioning requires provisioning enabled (mode %q)", cfg.Provisioning.Mode)
	}
	store, _, err := newSecretStore(opts.KubeconfigPath)
	if err != nil {
		return false, err
	}
	// Secret-only cleanup must not depend on GCS reachability.
	var buckets storage.BucketCreator
	if opts.DeleteBucket {
		gcsClient, cleanup, clientErr := storage.NewGCSClient(opts.Log)
		if clientErr != nil {
			return false, clientErr
		}
		defer cleanup()
		if buckets, err = storage.NewGCSBuckets(gcsClient, &cfg.Provisioning, opts.Log); err != nil {
			return false, err
		}
	}
	return storage.NewProvisioner(cfg, storage.ProvisionerDeps{Store: store, Buckets: buckets, Log: opts.Log}).DeprovisionTenant(ctx, opts.Slug, opts.DeleteBucket)
}
