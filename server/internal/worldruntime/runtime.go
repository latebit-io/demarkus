// Package worldruntime owns one logical world's request-serving state.
package worldruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/configwatch"
	"github.com/latebit-io/demarkus/server/internal/handler"
	"github.com/latebit-io/demarkus/server/internal/quicserve"
	"github.com/latebit-io/demarkus/server/internal/ratelimit"
)

const maxRateWaitBudget = 10 * time.Second

// Config defines one runtime and transfers backend ownership on success.
type Config struct {
	Name              string
	Store             backend.Store
	Catalog           backend.Catalog
	Views             backend.ViewProvider
	CloseBackend      func() error
	TokensFile        string
	DisableTokenWatch bool
	ReadOnly          bool
	RequestTimeout    time.Duration
	MaxConcurrent     int
	RateLimit         float64
	RateBurst         int
	Logger            *slog.Logger
}

// Runtime serves one world's streams with isolated auth and rate state.
type Runtime struct {
	handler        *handler.Handler
	tokens         *auth.Source
	requestTimeout time.Duration
	concurrent     chan struct{}
	limiter        *ratelimit.Limiter
	logger         *slog.Logger
	closeBackend   func() error

	watchCancel context.CancelFunc
	watchDone   chan struct{}

	mu      sync.Mutex
	closing bool
	active  sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

// New constructs a runtime and starts token watching when configured.
func New(config *Config) (*Runtime, error) {
	if config == nil {
		return nil, errors.New("world runtime: config is nil")
	}
	if config.Store == nil {
		return nil, errors.New("world runtime: store is nil")
	}
	if config.RateLimit < 0 || config.RateLimit > 0 && config.RateBurst <= 0 {
		return nil, fmt.Errorf("world runtime: invalid rate limit %g with burst %d", config.RateLimit, config.RateBurst)
	}
	if config.MaxConcurrent < 0 {
		return nil, fmt.Errorf("world runtime: max concurrent requests must not be negative: %d", config.MaxConcurrent)
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if config.Name != "" {
		logger = logger.With("world", config.Name)
	}
	tokens, err := auth.OpenSource(config.TokensFile)
	if err != nil {
		return nil, fmt.Errorf("world runtime: load tokens: %w", err)
	}
	runtime := &Runtime{
		tokens:         tokens,
		requestTimeout: config.RequestTimeout,
		logger:         logger,
		closeBackend:   config.CloseBackend,
		watchDone:      make(chan struct{}),
	}
	runtime.handler = &handler.Handler{
		Store:         config.Store,
		Catalog:       config.Catalog,
		Views:         config.Views,
		GetTokenStore: tokens.Current,
		Logger:        logger,
		ReadOnly:      config.ReadOnly,
	}
	if config.RateLimit > 0 {
		runtime.limiter = ratelimit.New(config.RateLimit, config.RateBurst)
	}
	if config.MaxConcurrent > 0 {
		runtime.concurrent = make(chan struct{}, config.MaxConcurrent)
	}
	if config.TokensFile == "" || config.DisableTokenWatch {
		close(runtime.watchDone)
		return runtime, nil
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	runtime.watchCancel = cancel
	watcher := &configwatch.Watcher{
		Target: config.TokensFile,
		Reload: tokens.Reload,
		Logger: logger,
	}
	go func() {
		defer close(runtime.watchDone)
		if err := watcher.Run(watchCtx); err != nil {
			logger.Warn("auth: token file watcher exited", "error", err)
		}
	}()
	return runtime, nil
}

// ServeStream applies world-local controls and dispatches one request.
func (r *Runtime) ServeStream(ctx context.Context, remote net.Addr, stream quicserve.Stream) {
	r.serveStream(ctx, remote, stream, r.logger)
}

// Endpoint binds one routed authority to request logs.
func (r *Runtime) Endpoint(authority string) quicserve.Endpoint {
	return &runtimeEndpoint{runtime: r, logger: r.logger.With("authority", authority)}
}

func (r *Runtime) serveStream(ctx context.Context, remote net.Addr, stream quicserve.Stream, logger *slog.Logger) {
	if !r.beginStream() {
		if err := stream.Close(); err != nil {
			logger.Debug("closing rejected stream", "error", err)
		}
		return
	}
	defer r.active.Done()

	if r.concurrent != nil {
		if !r.acquire(ctx, remote, stream, logger) {
			return
		}
		defer func() { <-r.concurrent }()
	}
	if r.limiter != nil {
		budget := r.requestTimeout
		if budget <= 0 {
			budget = maxRateWaitBudget
		}
		waitCtx, cancel := context.WithTimeout(ctx, budget)
		err := r.limiter.Wait(waitCtx, ratelimit.ExtractIP(remote))
		cancel()
		if err != nil {
			logger.Warn("rate limited", "ip", ratelimit.ExtractIP(remote), "error", err)
			if writeErr := writeRateLimited(stream); writeErr != nil {
				logger.Warn("writing rate-limited response", "ip", ratelimit.ExtractIP(remote), "error", writeErr)
			}
			if closeErr := stream.Close(); closeErr != nil {
				logger.Debug("closing rate-limited stream", "error", closeErr)
			}
			return
		}
	}
	if r.requestTimeout > 0 {
		if err := stream.SetReadDeadline(time.Now().Add(r.requestTimeout)); err != nil {
			logger.Debug("setting stream read deadline", "error", err)
		}
	}
	requestHandler := *r.handler
	requestHandler.Logger = logger
	requestHandler.HandleStream(stream)
}

func (r *Runtime) acquire(ctx context.Context, remote net.Addr, stream quicserve.Stream, logger *slog.Logger) bool {
	budget := r.requestTimeout
	if budget <= 0 {
		budget = maxRateWaitBudget
	}
	waitCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	select {
	case r.concurrent <- struct{}{}:
		return true
	case <-waitCtx.Done():
		ip := ratelimit.ExtractIP(remote)
		logger.Warn("concurrency limited", "ip", ip, "error", waitCtx.Err())
		if err := writeRateLimited(stream); err != nil {
			logger.Warn("writing concurrency-limited response", "ip", ip, "error", err)
		}
		if err := stream.Close(); err != nil {
			logger.Debug("closing concurrency-limited stream", "error", err)
		}
		return false
	}
}

type runtimeEndpoint struct {
	runtime *Runtime
	logger  *slog.Logger
}

func (endpoint *runtimeEndpoint) ServeStream(ctx context.Context, remote net.Addr, stream quicserve.Stream) {
	endpoint.runtime.serveStream(ctx, remote, stream, endpoint.logger)
}

func (r *Runtime) beginStream() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return false
	}
	r.active.Add(1)
	return true
}

// ReloadTokens atomically publishes a valid replacement token store.
func (r *Runtime) ReloadTokens() error {
	return r.tokens.Reload()
}

// LoadTokens reads a replacement token store without publishing it.
func (r *Runtime) LoadTokens() (*auth.TokenStore, error) {
	return r.tokens.Load()
}

// PublishTokens atomically installs a previously loaded token store.
func (r *Runtime) PublishTokens(store *auth.TokenStore) {
	r.tokens.Publish(store)
}

// Tokens returns the current immutable token-store snapshot.
func (r *Runtime) Tokens() *auth.TokenStore {
	return r.tokens.Current()
}

// Close stops runtime-local workers, drains streams, then closes the backend.
func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closing = true
		r.mu.Unlock()
		if r.watchCancel != nil {
			r.watchCancel()
		}
		<-r.watchDone
		r.active.Wait()
		if r.limiter != nil {
			r.limiter.Stop()
		}
		if r.closeBackend != nil {
			r.closeErr = r.closeBackend()
		}
	})
	return r.closeErr
}

func writeRateLimited(stream quicserve.Stream) error {
	_, err := protocol.Response{Status: protocol.StatusRateLimited}.WriteTo(stream)
	return err
}

var _ quicserve.Endpoint = (*Runtime)(nil)
