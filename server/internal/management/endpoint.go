package management

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Endpoint is the private health listener. A nil Endpoint is "none
// configured": every method is safe on it and reports nothing.
type Endpoint struct {
	listener net.Listener
	server   *http.Server
	status   *Health
}

// Listen opens the health listener on address; an empty address opens none.
func Listen(address string) (*Endpoint, error) {
	if address == "" {
		return nil, nil
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	status := &Health{}
	server := &http.Server{
		Handler:           status.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	return &Endpoint{listener: listener, server: server, status: status}, nil
}

// Serve answers probes as live and ready until Shutdown; the channel
// carries the server's exit, never for a nil endpoint.
func (e *Endpoint) Serve() <-chan error {
	result := make(chan error, 1)
	if e == nil {
		return result
	}
	go func() { result <- e.server.Serve(e.listener) }()
	e.status.SetLive(true)
	e.status.SetReady(true)
	return result
}

// Addr is the listener's address for the log, or none.
func (e *Endpoint) Addr() string {
	if e == nil {
		return "none"
	}
	return e.listener.Addr().String()
}

// SetReady changes aggregate readiness.
func (e *Endpoint) SetReady(ready bool) {
	if e != nil {
		e.status.SetReady(ready)
	}
}

// Shutdown stops answering probes once the process has drained.
func (e *Endpoint) Shutdown(logger *slog.Logger) {
	if e == nil {
		return
	}
	e.status.SetLive(false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.server.Shutdown(ctx); err != nil {
		logger.Warn("health shutdown incomplete", "error", err)
	}
}

// Close drops the listener at once, for a partial open.
func (e *Endpoint) Close(logger *slog.Logger) {
	if e == nil {
		return
	}
	if err := e.server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Warn("health server close failed", "error", err)
	}
}
