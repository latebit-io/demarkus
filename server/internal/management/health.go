// Package management serves private process health endpoints.
package management

import (
	"net/http"
	"sync/atomic"
)

// Health holds aggregate process liveness and readiness.
type Health struct {
	live  atomic.Bool
	ready atomic.Bool
}

// SetLive changes process liveness.
func (health *Health) SetLive(live bool) {
	health.live.Store(live)
}

// SetReady changes aggregate readiness.
func (health *Health) SetReady(ready bool) {
	health.ready.Store(ready)
}

// Handler returns the private health HTTP handler.
func (health *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", probe(&health.live))
	mux.HandleFunc("HEAD /livez", probe(&health.live))
	mux.HandleFunc("GET /readyz", probe(&health.ready))
	mux.HandleFunc("HEAD /readyz", probe(&health.ready))
	return mux
}

func probe(state *atomic.Bool) http.HandlerFunc {
	return func(response http.ResponseWriter, _ *http.Request) {
		if !state.Load() {
			http.Error(response, "unavailable", http.StatusServiceUnavailable)
			return
		}
		response.WriteHeader(http.StatusOK)
	}
}
