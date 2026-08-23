package management

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealth(t *testing.T) {
	var health Health
	handler := health.Handler()
	assertStatus(t, handler, "/livez", http.StatusServiceUnavailable)
	assertStatus(t, handler, "/readyz", http.StatusServiceUnavailable)

	health.SetLive(true)
	assertStatus(t, handler, "/livez", http.StatusOK)
	assertStatus(t, handler, "/readyz", http.StatusServiceUnavailable)

	health.SetReady(true)
	assertStatus(t, handler, "/readyz", http.StatusOK)
	health.SetLive(false)
	health.SetReady(false)
	assertStatus(t, handler, "/livez", http.StatusServiceUnavailable)
	assertStatus(t, handler, "/readyz", http.StatusServiceUnavailable)
}

func TestHealthRejectsOtherRoutesAndMethods(t *testing.T) {
	var health Health
	handler := health.Handler()
	for _, method := range []string{http.MethodHead, http.MethodPost} {
		request := httptest.NewRequest(method, "/livez", http.NoBody)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /livez status = %d, want %d", method, response.Code, http.StatusMethodNotAllowed)
		}
	}
	assertStatus(t, handler, "/unknown", http.StatusNotFound)
}

func assertStatus(t *testing.T, handler http.Handler, path string, want int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("GET %s status = %d, want %d", path, response.Code, want)
	}
}
