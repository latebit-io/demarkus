// Package worldruntime owns one logical world's request-serving state.
package worldruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/configwatch"
	"github.com/latebit-io/demarkus/server/internal/fanout"
	"github.com/latebit-io/demarkus/server/internal/handler"
	"github.com/latebit-io/demarkus/server/internal/quicserve"
	"github.com/latebit-io/demarkus/server/internal/ratelimit"
	"github.com/latebit-io/demarkus/server/logthrottle"
)

const maxRateWaitBudget = 10 * time.Second

// ErrClosing is Exchange's answer once the runtime is closing.
var ErrClosing = errors.New("world runtime: closing")

// Config defines one runtime and transfers backend ownership on success.
type Config struct {
	Name         string
	Store        backend.Store
	CloseBackend func() error
	TokensFile   string
	// StaticTokensFile holds operator-owned entries merged with TokensFile.
	StaticTokensFile string
	// OptionalTokensFiles opens the world when a tokens file is missing and
	// reloads it once it appears; see auth.SourceConfig.Optional.
	OptionalTokensFiles bool
	DisableTokenWatch   bool
	ReadOnly            bool
	RequestTimeout      time.Duration
	MaxConcurrent       int
	RateLimit           float64
	RateBurst           int
	Logger              *slog.Logger
	// Changes serves WATCH and is closed with the runtime; nil leaves the
	// verb unsupported.
	Changes *changefeed.Hub
	// MaxWatches caps open watches per world; zero takes the default.
	// MaxWatchesPerConn caps them per connection; zero leaves half of
	// MaxStreams, the connection's stream limit, to requests.
	MaxWatches        int
	MaxWatchesPerConn int
	MaxStreams        int
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
	changes        *changefeed.Hub
	watches        *fanout.Fanout

	// A flood of refusals logs once per window per kind, not per request.
	rateRefusals        *logthrottle.Throttle
	concurrencyRefusals *logthrottle.Throttle

	watchCancel context.CancelFunc
	watchDone   sync.WaitGroup

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
	if config.MaxWatches < 0 || config.MaxWatchesPerConn < 0 {
		return nil, fmt.Errorf("world runtime: watch limits must not be negative: %d, %d", config.MaxWatches, config.MaxWatchesPerConn)
	}
	logger := config.Logger
	if logger == nil {
		return nil, errors.New("world runtime: logger is nil")
	}
	if config.Name != "" {
		logger = logger.With("world", config.Name)
	}
	// Every caller watches by directory, so a split pair would silently
	// stop reloading after the watcher exits.
	if config.StaticTokensFile != "" && config.TokensFile != "" && filepath.Dir(config.StaticTokensFile) != filepath.Dir(config.TokensFile) {
		return nil, fmt.Errorf("world runtime: static tokens file %q must share the directory of %q", config.StaticTokensFile, config.TokensFile)
	}
	sourceConfig := auth.SourceConfig{
		TokensFile:       config.TokensFile,
		StaticTokensFile: config.StaticTokensFile,
		Optional:         config.OptionalTokensFiles,
		Logger:           logger,
	}
	tokens, err := auth.OpenSource(sourceConfig)
	if err != nil {
		return nil, fmt.Errorf("world runtime: load tokens: %w", err)
	}
	runtime := &Runtime{
		tokens:         tokens,
		requestTimeout: config.RequestTimeout,
		logger:         logger,
		closeBackend:   config.CloseBackend,
		changes:        config.Changes,
	}
	if config.Changes != nil {
		perConn := config.MaxWatchesPerConn
		if perConn == 0 {
			perConn = max(1, config.MaxStreams/2)
		}
		runtime.watches, err = fanout.New(fanout.Config{
			Hub: config.Changes, Tokens: tokens.Current, Logger: logger, MaxWatches: config.MaxWatches, MaxWatchesPerConn: perConn,
		})
		if err != nil {
			return nil, fmt.Errorf("world runtime: %w", err)
		}
	}
	runtime.handler, err = handler.New(handler.Config{
		Store:         config.Store,
		GetTokenStore: tokens.Current,
		Logger:        logger,
		ReadOnly:      config.ReadOnly,
		Watches:       runtime.watches,
	})
	if err != nil {
		return nil, fmt.Errorf("world runtime: %w", err)
	}
	if config.RateLimit > 0 {
		runtime.limiter = ratelimit.New(config.RateLimit, config.RateBurst)
		runtime.rateRefusals = logthrottle.New(logthrottle.DefaultWindow)
	}
	if config.MaxConcurrent > 0 {
		runtime.concurrent = make(chan struct{}, config.MaxConcurrent)
		runtime.concurrencyRefusals = logthrottle.New(logthrottle.DefaultWindow)
	}
	files := sourceConfig.Files()
	if config.DisableTokenWatch || len(files) == 0 {
		return runtime, nil
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	runtime.watchCancel = cancel
	watcher := &configwatch.Watcher{
		Targets: files,
		Reload:  tokens.Reload,
		Logger:  logger,
	}
	runtime.watchDone.Go(func() {
		if err := watcher.Run(watchCtx); err != nil {
			logger.Warn("auth: token file watcher exited", "error", err)
		}
	})
	return runtime, nil
}

// ServeStream applies world-local controls and dispatches one request.
func (r *Runtime) ServeStream(ctx context.Context, remote net.Addr, stream quicserve.Stream) {
	(&runtimeEndpoint{runtime: r, logger: r.logger}).serve(ctx, remote, stream, nil)
}

// Endpoint binds one routed authority to request logs.
func (r *Runtime) Endpoint(authority string) quicserve.Endpoint {
	return &runtimeEndpoint{runtime: r, logger: r.logger.With("authority", authority), authority: authority}
}

// Gates builds the gate of a bearer connection to authority, once per
// connection; an error refuses the connection.
type Gates = func(authority string) (protocol.Gate, error)

// Gatable is a routed endpoint that also serves bearer connections.
type Gatable interface {
	Gated(gates Gates) (quicserve.Endpoint, error)
}

// exchange answers one request in process under the same controls as a
// stream: the closing gate, the concurrency slot, the rate limit and the
// request deadline. WATCH needs a stream and is answered bad-request.
func (r *Runtime) exchange(ctx context.Context, remote net.Addr, req protocol.Request, logger *slog.Logger) (protocol.Response, error) {
	if req.Verb == protocol.VerbWatch {
		return protocol.Response{Status: protocol.StatusBadRequest, Body: "WATCH needs a stream"}, nil
	}
	if !r.beginStream() {
		return protocol.Response{}, ErrClosing
	}
	defer r.active.Done()
	held, admitted := r.admit(ctx, remote, logger)
	if !admitted {
		return protocol.Response{Status: protocol.StatusRateLimited}, nil
	}
	defer held.release()
	requestCtx := ctx
	if r.requestTimeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, r.requestTimeout)
		defer cancel()
	}
	var out bytes.Buffer
	r.handler.WithLogger(logger).Serve(requestCtx, &out, req)
	resp, err := protocol.ParseResponse(&out)
	if err != nil {
		return protocol.Response{}, fmt.Errorf("read response: %w", err)
	}
	return resp, nil
}

// admit takes the concurrency slot and the rate-limit token for one
// request, or refuses it with nothing held.
func (r *Runtime) admit(ctx context.Context, remote net.Addr, logger *slog.Logger) (held slot, admitted bool) {
	if r.concurrent != nil && !r.acquire(ctx, remote, logger) {
		return slot{}, false
	}
	held = slot{ch: r.concurrent}
	if r.limiter == nil {
		return held, true
	}
	waitCtx, cancel := context.WithTimeout(ctx, r.budget())
	err := r.limiter.Wait(waitCtx, ratelimit.ExtractIP(remote))
	cancel()
	if err != nil {
		held.release()
		if logIt, suppressed := r.rateRefusals.Allow(); logIt {
			logger.Warn("rate limited", "ip", ratelimit.ExtractIP(remote), "error", err, "suppressed", suppressed)
		}
		return slot{}, false
	}
	return held, true
}

// slot is one concurrency slot, released at most once; a nil channel is no
// limit.
type slot struct{ ch chan struct{} }

func (s *slot) release() {
	if s.ch != nil {
		<-s.ch
		s.ch = nil
	}
}

// boundedWriter gives every write its own deadline.
type boundedWriter struct {
	stream quicserve.Stream
	budget time.Duration
}

func (w boundedWriter) Write(p []byte) (int, error) {
	if err := w.stream.SetWriteDeadline(time.Now().Add(w.budget)); err != nil {
		return 0, fmt.Errorf("set write deadline: %w", err)
	}
	return w.stream.Write(p)
}

// budget bounds one wait or write: the request timeout, or a fixed one when
// requests are unbounded, so a peer that stops reading cannot hold a stream.
func (r *Runtime) budget() time.Duration {
	if r.requestTimeout > 0 {
		return r.requestTimeout
	}
	return maxRateWaitBudget
}

// Watches reports the world's open watches.
func (r *Runtime) Watches() int {
	if r.watches == nil {
		return 0
	}
	return r.watches.Watches()
}

func (r *Runtime) acquire(ctx context.Context, remote net.Addr, logger *slog.Logger) bool {
	waitCtx, cancel := context.WithTimeout(ctx, r.budget())
	defer cancel()
	select {
	case r.concurrent <- struct{}{}:
		return true
	case <-waitCtx.Done():
		if logIt, suppressed := r.concurrencyRefusals.Allow(); logIt {
			logger.Warn("concurrency limited", "ip", ratelimit.ExtractIP(remote), "error", waitCtx.Err(), "suppressed", suppressed)
		}
		return false
	}
}

// Exchanger serves requests in process; every routed endpoint is one.
// ServeRequest answers req on stream as if stream had carried it, then
// closes stream: the way to serve a WATCH, which outlives one response.
type Exchanger interface {
	Exchange(ctx context.Context, remote net.Addr, req protocol.Request) (protocol.Response, error)
	ServeRequest(ctx context.Context, remote net.Addr, req protocol.Request, stream quicserve.Stream)
}

type runtimeEndpoint struct {
	runtime   *Runtime
	logger    *slog.Logger
	authority string
	// gate is set on a bearer connection's copy only.
	gate protocol.Gate
}

// serve answers one stream: parsed is its request when the caller already
// holds one, else the request is read from stream.
func (endpoint *runtimeEndpoint) serve(ctx context.Context, remote net.Addr, stream quicserve.Stream, parsed *protocol.Request) {
	r, logger := endpoint.runtime, endpoint.logger
	if !r.beginStream() {
		if err := stream.Close(); err != nil {
			logger.Debug("closing rejected stream", "error", err)
		}
		return
	}
	defer r.active.Done()
	// The response is written or failed by then; a close error changes nothing.
	defer func() {
		if err := stream.Close(); err != nil {
			logger.Debug("closing stream", "error", err)
		}
	}()

	held, admitted := r.admit(ctx, remote, logger)
	if !admitted {
		if err := r.writeRateLimited(stream); err != nil {
			logger.Warn("writing rate-limited response", "ip", ratelimit.ExtractIP(remote), "error", err)
		}
		return
	}
	defer held.release()
	requestCtx := ctx
	if r.requestTimeout > 0 {
		// The write bound keeps a client that stops reading from pinning the
		// read view and a concurrency slot; store calls share the deadline.
		deadline := time.Now().Add(r.requestTimeout)
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
		if err := stream.SetReadDeadline(deadline); err != nil {
			logger.Debug("setting stream read deadline", "error", err)
		}
		if err := stream.SetWriteDeadline(deadline); err != nil {
			logger.Debug("setting stream write deadline", "error", err)
		}
	}
	h := r.handler.WithLogger(logger)
	var req protocol.Request
	if parsed != nil {
		req = *parsed
	} else {
		var ok bool
		if req, ok = h.ReadRequest(stream); !ok {
			return
		}
	}
	if endpoint.gate != nil {
		grant, err := endpoint.gate(requestCtx, req)
		// The bearer is not a capability token; the handler never sees it.
		delete(req.Metadata, "auth")
		if err != nil {
			h.Deny(stream, req, gateDenial(err))
			return
		}
		ctx, requestCtx = protocol.WithGrant(ctx, grant), protocol.WithGrant(requestCtx, grant)
	}
	if req.Verb == protocol.VerbWatch && r.watches != nil {
		// A watch is admitted under the fan-out's caps, not the request slots,
		// and lives as long as the connection; each block write is bounded on its own.
		held.release()
		h.Serve(ctx, boundedWriter{stream: stream, budget: r.budget()}, req)
		return
	}
	h.Serve(requestCtx, stream, req)
}

func (endpoint *runtimeEndpoint) ServeStream(ctx context.Context, remote net.Addr, stream quicserve.Stream) {
	endpoint.serve(ctx, remote, stream, nil)
}

// Gated is this endpoint for one bearer connection: every request passes
// the gate gates builds for the endpoint's authority.
func (endpoint *runtimeEndpoint) Gated(gates Gates) (quicserve.Endpoint, error) {
	gate, err := gates(endpoint.authority)
	if err != nil {
		return nil, err
	}
	gated := *endpoint
	gated.gate = gate
	return &gated, nil
}

// gateDenial names a gate's refusal in the handler's terms.
func gateDenial(err error) error {
	if errors.Is(err, protocol.ErrNotPermitted) {
		return auth.ErrNotPermitted
	}
	return auth.ErrInvalidToken
}

func (endpoint *runtimeEndpoint) Exchange(ctx context.Context, remote net.Addr, req protocol.Request) (protocol.Response, error) {
	return endpoint.runtime.exchange(ctx, remote, req, endpoint.logger)
}

func (endpoint *runtimeEndpoint) ServeRequest(ctx context.Context, remote net.Addr, req protocol.Request, stream quicserve.Stream) {
	endpoint.serve(ctx, remote, stream, &req)
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
func (r *Runtime) PublishTokens(store *auth.TokenStore) error {
	return r.tokens.Publish(store)
}

// Tokens returns the current immutable token-store snapshot.
func (r *Runtime) Tokens() *auth.TokenStore {
	return r.tokens.Current()
}

// Drain tells every open watch closing, so a listener shutdown that waits
// for streams is not held by subscriptions. Requests are still served.
func (r *Runtime) Drain() {
	if r.changes != nil {
		r.changes.Close()
	}
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
		r.watchDone.Wait()
		r.Drain()
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

// writeRateLimited refuses before the request deadlines exist, so it bounds
// its own write: a peer that stops reading must not hold the stream open.
func (r *Runtime) writeRateLimited(stream quicserve.Stream) error {
	if err := stream.SetWriteDeadline(time.Now().Add(r.budget())); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	_, err := protocol.Response{Status: protocol.StatusRateLimited}.WriteTo(stream)
	return err
}

var (
	_ quicserve.Endpoint = (*Runtime)(nil)
	_ Exchanger          = (*runtimeEndpoint)(nil)
	_ Gatable            = (*runtimeEndpoint)(nil)
)
