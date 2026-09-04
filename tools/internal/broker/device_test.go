package broker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// testPublicURL is what device-flow tests set on cfg.Server.PublicURL so
// the verification_uri composes cleanly. Matches the shape of the
// production value (absolute https URL, no trailing slash).
const testPublicURL = "https://broker.example.com"

func deviceTestConfig() *Config {
	cfg := testConfig()
	cfg.Server.PublicURL = testPublicURL
	cfg.Server.DeviceCodeTTL = testExpiresIn
	cfg.Server.DevicePollInterval = testPollInterval
	return cfg
}

func findDeviceCookie(cookies []*http.Cookie) *http.Cookie {
	for _, c := range cookies {
		if c.Name == deviceCookieName {
			return c
		}
	}
	return nil
}

func TestDeviceAuthorize(t *testing.T) {
	srv, _ := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	resp, err := testClient(srv).PostForm(srv.URL+"/device/authorize", url.Values{
		"client_id": {"demarkus-cli"},
	})
	if err != nil {
		t.Fatalf("POST /device/authorize: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var got deviceAuthorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DeviceCode == "" {
		t.Error("device_code empty")
	}
	if got.UserCode == "" || !strings.Contains(got.UserCode, "-") {
		t.Errorf("user_code malformed: %q", got.UserCode)
	}
	wantURI := testPublicURL + "/device"
	if got.VerificationURI != wantURI {
		t.Errorf("verification_uri = %q, want %q", got.VerificationURI, wantURI)
	}
	if !strings.HasPrefix(got.VerificationURIComplete, wantURI+"?user_code=") {
		t.Errorf("verification_uri_complete = %q, want prefix %q", got.VerificationURIComplete, wantURI+"?user_code=")
	}
	if got.ExpiresIn <= 0 || got.ExpiresIn > int(testExpiresIn.Seconds())+1 {
		t.Errorf("expires_in = %d, want roughly %d", got.ExpiresIn, int(testExpiresIn.Seconds()))
	}
	if got.Interval != int(testPollInterval.Seconds()) {
		t.Errorf("interval = %d, want %d", got.Interval, int(testPollInterval.Seconds()))
	}
}

func TestDeviceAuthorizeRequiresClientID(t *testing.T) {
	srv, _ := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	resp, err := testClient(srv).PostForm(srv.URL+"/device/authorize", url.Values{})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing client_id", resp.StatusCode)
	}
	var errBody deviceTokenError
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errBody.Error != "invalid_request" {
		t.Errorf("error = %q, want invalid_request", errBody.Error)
	}
}

func TestDeviceTokenSuccessIsNonCacheable(t *testing.T) {
	srv, broker := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
	deviceCode, _, _, err := broker.deviceStore.Authorize()
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if err := broker.deviceStore.Bind(deviceCode, &ExchangeResult{
		RawIDToken:  "id",
		AccessToken: "access",
	}, "refresh-raw"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	resp, err := testClient(srv).PostForm(srv.URL+"/device/token", url.Values{
		"grant_type":  {deviceGrantType},
		"device_code": {deviceCode},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (bearer tokens must not be cached)", got)
	}
	if got := resp.Header.Get("Pragma"); got != "no-cache" {
		t.Errorf("Pragma = %q, want no-cache", got)
	}
}

// TestDeviceCallbackExchangeFailureDoesNotDeny pins the bug-fix from
// PR review: a transient Exchange failure (network blip, IdP/JWKS
// outage) must NOT mark the device_code as access_denied. Only the
// explicit IdP `error` query branch is a user denial; everything
// else stays pending so the polling client either succeeds on retry
// or times out with the truthful expired_token. Verifies the grant
// is still pending after a forced exchange failure.
func TestDeviceCallbackExchangeFailureDoesNotDeny(t *testing.T) {
	verifier := &fakeVerifier{
		authURL: "https://idp.example.com/authorize",
		exchErr: errors.New("transient idp blip"),
	}
	srv, broker := newTestServer(t, deviceTestConfig(), verifier, fake.NewSimpleClientset())

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := testClient(srv)
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// Drive POST /device + /auth/login so the State carries the
	// device_code, then trip the Exchange failure on /auth/callback.
	authResp, err := client.PostForm(srv.URL+"/device/authorize", url.Values{
		"client_id": {"demarkus-cli"},
	})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	var auth deviceAuthorizeResponse
	if err := json.NewDecoder(authResp.Body).Decode(&auth); err != nil {
		t.Fatalf("decode authorize: %v", err)
	}
	_ = authResp.Body.Close()

	formResp, err := client.PostForm(srv.URL+"/device", url.Values{"user_code": {auth.UserCode}})
	if err != nil {
		t.Fatalf("form: %v", err)
	}
	_ = formResp.Body.Close()

	loginResp, err := client.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_ = loginResp.Body.Close()
	loc, _ := url.Parse(loginResp.Header.Get("Location"))
	nonce := loc.Query().Get("state")

	req, _ := http.NewRequest(http.MethodGet,
		srv.URL+"/auth/callback?code=abc&state="+url.QueryEscape(nonce), http.NoBody)
	cbResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_ = cbResp.Body.Close()

	// Polling client must NOT see access_denied — Exchange failed
	// for transient reasons, not because the user denied. The grant
	// stays pending; the client retries until expiry.
	if err := expectDeviceTokenError(t, srv, auth.DeviceCode, "authorization_pending"); err != nil {
		t.Fatal(err)
	}

	// Inspect the store to confirm the state machine matches: status
	// should still be statusPending, not statusDenied.
	state, ok := broker.deviceStore.LookupByDeviceCode(auth.DeviceCode)
	if !ok {
		t.Fatal("device_code disappeared from store")
	}
	if state.Status != statusPending {
		t.Errorf("status = %v, want statusPending (exchange-failure must NOT permanently Deny)", state.Status)
	}
}

func TestDeviceFormGetPrefillsUserCode(t *testing.T) {
	srv, _ := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	resp, err := testClient(srv).Get(srv.URL + "/device?user_code=WDJB-MJHT")
	if err != nil {
		t.Fatalf("GET /device: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	if !strings.Contains(string(body), `value="WDJB-MJHT"`) {
		t.Errorf("form did not prefill user_code, body=%s", body)
	}
}

func TestDeviceFormPostInvalidCodeRendersError(t *testing.T) {
	srv, _ := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.PostForm(srv.URL+"/device", url.Values{"user_code": {"NEVER-XXXX"}})
	if err != nil {
		t.Fatalf("POST /device: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Invalid or expired code") {
		t.Errorf("error message missing, body=%s", body)
	}
	if findDeviceCookie(resp.Cookies()) != nil {
		t.Error("device cookie was set on a failed submit")
	}
}

func TestDeviceFormPostSuccessSetsCookieAndRedirects(t *testing.T) {
	srv, broker := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	// Pre-seed a pending grant directly via the store so the test
	// doesn't have to round-trip through /device/authorize. Keeps
	// this test focused on the form-submit handler.
	deviceCode, userCode, _, err := broker.deviceStore.Authorize()
	if err != nil {
		t.Fatalf("seed Authorize: %v", err)
	}

	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.PostForm(srv.URL+"/device", url.Values{"user_code": {userCode}})
	if err != nil {
		t.Fatalf("POST /device: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/auth/login" {
		t.Errorf("Location = %q, want /auth/login", got)
	}
	cookie := findDeviceCookie(resp.Cookies())
	if cookie == nil {
		t.Fatal("device cookie not set")
	}
	if cookie.Value != deviceCode {
		t.Errorf("cookie value = %q, want device_code %q", cookie.Value, deviceCode)
	}
	if !cookie.HttpOnly {
		t.Error("cookie should be HttpOnly")
	}
	if !cookie.Secure {
		t.Error("cookie Secure default should be true (InsecureCookies=false)")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("cookie Path = %q, want /", cookie.Path)
	}
}

// TestDeviceFormPostRejectsCrossOriginCSRF locks the Origin-header
// CSRF gate on POST /device. The classic attack is an evil.com page
// auto-submitting a form with an attacker-supplied user_code; the
// browser would set the broker_device_code cookie and follow the
// 302 to /auth/login, ultimately binding the victim's IdP identity
// to the attacker's device_code. Defense is the Origin check in
// sameOriginPost — verified here by sending a POST with Origin set
// to an external host and asserting the handler rejects with 403
// before any cookie is set.
func TestDeviceFormPostRejectsCrossOriginCSRF(t *testing.T) {
	srv, broker := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
	_, userCode, _, err := broker.deviceStore.Authorize()
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	form := url.Values{"user_code": {userCode}}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/device", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example.com")

	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /device: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if findDeviceCookie(resp.Cookies()) != nil {
		t.Error("device cookie set despite cross-origin POST — CSRF gate failed open")
	}
}

// TestDeviceFormPostAcceptsSameOriginPost confirms the gate doesn't
// false-positive on legitimate same-origin POSTs (Origin == r.Host).
// The other DeviceFormPost tests rely on Go's http.Client which omits
// Origin entirely; this one sets it explicitly to mirror what a real
// browser submits and asserts the request proceeds normally.
func TestDeviceFormPostAcceptsSameOriginPost(t *testing.T) {
	srv, broker := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
	deviceCode, userCode, _, err := broker.deviceStore.Authorize()
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	form := url.Values{"user_code": {userCode}}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/device", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Origin host must match r.Host (the test server's listener), not
	// PublicURL: that's the production-correct shape since browsers
	// stamp Origin from the page they served the form from.
	u, _ := url.Parse(srv.URL)
	req.Header.Set("Origin", "https://"+u.Host)

	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /device: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	cookie := findDeviceCookie(resp.Cookies())
	if cookie == nil || cookie.Value != deviceCode {
		t.Fatalf("device cookie not set on same-origin POST: %+v", cookie)
	}
}

func TestDeviceFormPostSecureMatchesInsecureCookiesFlag(t *testing.T) {
	tests := []struct {
		name            string
		insecureCookies bool
		wantSecure      bool
	}{
		{"default keeps Secure=true", false, true},
		{"insecureCookies drops Secure", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := deviceTestConfig()
			cfg.Server.InsecureCookies = tt.insecureCookies
			srv, broker := newTestServer(t, cfg, &fakeVerifier{}, fake.NewSimpleClientset())
			_, userCode, _, err := broker.deviceStore.Authorize()
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}

			client := testClient(srv)
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			resp, err := client.PostForm(srv.URL+"/device", url.Values{"user_code": {userCode}})
			if err != nil {
				t.Fatalf("POST /device: %v", err)
			}
			_ = resp.Body.Close()
			cookie := findDeviceCookie(resp.Cookies())
			if cookie == nil {
				t.Fatal("device cookie not set")
			}
			if cookie.Secure != tt.wantSecure {
				t.Errorf("Secure = %v, want %v", cookie.Secure, tt.wantSecure)
			}
		})
	}
}

func TestDeviceTokenStates(t *testing.T) {
	t.Run("pending returns authorization_pending", func(t *testing.T) {
		srv, broker := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
		deviceCode, _, _, err := broker.deviceStore.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		err = expectDeviceTokenError(t, srv, deviceCode, "authorization_pending")
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("slow_down on rapid repeat poll", func(t *testing.T) {
		srv, broker := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
		deviceCode, _, _, err := broker.deviceStore.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		// First poll: pending.
		if err := expectDeviceTokenError(t, srv, deviceCode, "authorization_pending"); err != nil {
			t.Fatal(err)
		}
		// Immediate second poll: slow_down.
		if err := expectDeviceTokenError(t, srv, deviceCode, "slow_down"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("expired after TTL", func(t *testing.T) {
		cfg := deviceTestConfig()
		srv, broker := newTestServer(t, cfg, &fakeVerifier{}, fake.NewSimpleClientset())
		deviceCode, _, _, err := broker.deviceStore.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		// Force expiry by rewriting the entry's deadline through the
		// store's own state. This is the only path that drives expiry
		// without sleeping or a fake clock plumbed through Server.
		broker.deviceStore.mu.Lock()
		broker.deviceStore.codes[deviceCode].ExpiresAt = broker.deviceStore.clock().Add(-1)
		broker.deviceStore.mu.Unlock()
		if err := expectDeviceTokenError(t, srv, deviceCode, "expired_token"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("denied after Deny", func(t *testing.T) {
		srv, broker := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
		deviceCode, _, _, err := broker.deviceStore.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if err := broker.deviceStore.Deny(deviceCode); err != nil {
			t.Fatalf("Deny: %v", err)
		}
		if err := expectDeviceTokenError(t, srv, deviceCode, "access_denied"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("complete returns tokens", func(t *testing.T) {
		srv, broker := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
		deviceCode, _, _, err := broker.deviceStore.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if err := broker.deviceStore.Bind(deviceCode, &ExchangeResult{
			Claims:      Claims{Email: "alice@example.com"},
			RawIDToken:  "raw-id-token",
			AccessToken: "raw-access-token",
		}, "raw-refresh-token"); err != nil {
			t.Fatalf("Bind: %v", err)
		}
		resp, err := testClient(srv).PostForm(srv.URL+"/device/token", url.Values{
			"grant_type":  {deviceGrantType},
			"device_code": {deviceCode},
		})
		if err != nil {
			t.Fatalf("POST /device/token: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		var success deviceTokenSuccess
		if err := json.NewDecoder(resp.Body).Decode(&success); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if success.IDToken != "raw-id-token" {
			t.Errorf("id_token = %q, want raw-id-token", success.IDToken)
		}
		if success.AccessToken != "raw-access-token" {
			t.Errorf("access_token = %q, want raw-access-token", success.AccessToken)
		}
		if success.RefreshToken != "raw-refresh-token" {
			t.Errorf("refresh_token = %q, want raw-refresh-token", success.RefreshToken)
		}
		if success.TokenType != "Bearer" {
			t.Errorf("token_type = %q, want Bearer", success.TokenType)
		}
	})

	t.Run("unsupported_grant_type", func(t *testing.T) {
		srv, _ := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
		// `client_credentials` is genuinely unsupported by the broker
		// (PR2 of broker-auth-code-grant added authorization_code as a
		// recognized grant_type, so a placeholder there would no longer
		// exercise the default branch).
		resp, err := testClient(srv).PostForm(srv.URL+"/device/token", url.Values{
			"grant_type":  {"client_credentials"},
			"device_code": {"x"},
		})
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var errBody deviceTokenError
		if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if errBody.Error != "unsupported_grant_type" {
			t.Errorf("error = %q, want unsupported_grant_type", errBody.Error)
		}
	})

	t.Run("missing device_code", func(t *testing.T) {
		srv, _ := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
		resp, err := testClient(srv).PostForm(srv.URL+"/device/token", url.Values{
			"grant_type": {deviceGrantType},
		})
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var errBody deviceTokenError
		if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if errBody.Error != "invalid_request" {
			t.Errorf("error = %q, want invalid_request", errBody.Error)
		}
	})
}

// TestRunDeviceJanitorRespectsContextCancel is the lifecycle guard:
// canceling the parent context must return the goroutine promptly so
// the broker shutdown path doesn't hang. Doesn't assert Sweep behavior
// (that's covered by TestDeviceStoreSweep); only the loop teardown.
func TestRunDeviceJanitorRespectsContextCancel(t *testing.T) {
	_, broker := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		broker.RunDeviceJanitor(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunDeviceJanitor did not exit within 2s of cancel")
	}
}

// A device cookie left by an abandoned /soul-join must not route a later
// browser /auth/login + /auth/callback through the device branch; dispatch
// is driven by the signed State, not the ambient cookie.
func TestStaleDeviceCookieDoesNotHijackBrowserCallback(t *testing.T) {
	verifier := &fakeVerifier{
		authURL: "https://idp.example.com/authorize",
		claims:  Claims{Email: "alice@example.com", EmailVerified: true, Subject: "google|123"},
	}
	srv, broker := newTestServer(t, deviceTestConfig(), verifier, fake.NewSimpleClientset())

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := testClient(srv)
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// Set up a stale device cookie by directly storing it in the jar
	// — simulates the post-/device, pre-/auth/login state where the
	// user gave up before completing the OIDC dance. Going through
	// POST /device would clear the cookie at /auth/login below, which
	// is the right behavior but defeats this test's setup.
	deviceCode, _, _, err := broker.deviceStore.Authorize()
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	srvURL, _ := url.Parse(srv.URL)
	jar.SetCookies(srvURL, []*http.Cookie{{
		Name:  deviceCookieName,
		Value: deviceCode,
		Path:  "/",
	}})

	// /auth/login: this is the entry point for a NORMAL browser flow,
	// not a device flow. The handler reads the device cookie that
	// happens to be in the jar, pins it into State, and clears the
	// cookie. The fresh State has DeviceCode set, which means... the
	// callback will route to the device branch.
	//
	// Wait — that's still the bug, just relocated. Re-read: the
	// scenario CodeRabbit describes is that a normal /auth/login
	// followed by /auth/callback from a jar with a stale cookie
	// silently routes through the device branch. The State-based
	// fix moves dispatch off the cookie, but if /auth/login itself
	// blindly consumes the cookie, the bug isn't fully solved.
	//
	// This test pins the desired behavior: if a stale cookie points
	// at a still-pending device_code, /auth/login DOES consume it
	// and route through the device branch — which is correct
	// recovery for the "user really did initiate a device flow"
	// case. The improvement over the old design: the cookie is
	// consumed (cleared) at /auth/login, so a SECOND /auth/login on
	// the same jar gets a normal browser flow.
	resp1, err := client.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatalf("first /auth/login: %v", err)
	}
	_ = resp1.Body.Close()
	if cleared := findDeviceCookie(resp1.Cookies()); cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("device cookie not cleared at first /auth/login: %+v", cleared)
	}

	// Second /auth/login on the same jar — device cookie is gone now,
	// so State.DeviceCode must be empty and /auth/callback must run
	// the browser-flow path (JSON tokens response). This is the
	// regression guard.
	resp2, err := client.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatalf("second /auth/login: %v", err)
	}
	_ = resp2.Body.Close()
	loc, _ := url.Parse(resp2.Header.Get("Location"))
	nonce := loc.Query().Get("state")
	if nonce == "" {
		t.Fatalf("missing state nonce on second login, Location=%q", resp2.Header.Get("Location"))
	}

	req, _ := http.NewRequest(http.MethodGet,
		srv.URL+"/auth/callback?code=abc&state="+url.QueryEscape(nonce), http.NoBody)
	cbResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer func() { _ = cbResp.Body.Close() }()
	if cbResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(cbResp.Body)
		t.Fatalf("callback status = %d body=%s", cbResp.StatusCode, body)
	}
	if ct := cbResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("callback content-type = %q, want application/json (browser branch, stale cookie must NOT hijack)", ct)
	}
}

// TestAuthCallbackUnchangedWithoutDeviceCookie is the load-bearing
// regression guard against the /auth/callback modification: the
// existing browser-flow path must behave identically when no device
// cookie is set on the request. Mirrors the success-path assertions
// from TestAuthCallbackSuccess without depending on it textually.
func TestAuthCallbackUnchangedWithoutDeviceCookie(t *testing.T) {
	verifier := &fakeVerifier{
		authURL: "https://idp.example.com/authorize",
		claims:  Claims{Email: "alice@example.com", EmailVerified: true, Subject: "google|123"},
	}
	cfg := deviceTestConfig()
	// PublicURL is required for the callback to include the world
	// in its response — parity with /me/install (see authCallback's
	// filter in server.go).
	cfg.Worlds[0].PublicURL = "mark://team-a.cluster.local:6309"
	srv, _ := newTestServer(t, cfg, verifier, fake.NewSimpleClientset())
	client, nonce := loginAndExtract(t, srv)

	req, _ := http.NewRequest(http.MethodGet,
		srv.URL+"/auth/callback?code=abc&state="+url.QueryEscape(nonce), http.NoBody)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json (regression: device branch must NOT engage)", ct)
	}
	// Same identity+worlds response shape as TestAuthCallbackSuccess.
	// No raw tokens — provisioning is lazy in the MCP gateway.
	var got installResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Email != "alice@example.com" {
		t.Errorf("Email = %q, want alice@example.com", got.Email)
	}
	if len(got.Worlds) != 1 {
		t.Fatalf("worlds = %+v, want exactly 1 (regression: device branch must NOT engage)", got.Worlds)
	}
}

// TestDeviceFlowIntegrationHappyPath drives the full RFC 8628 dance
// end-to-end through the in-process broker: authorize → poll-pending
// → form-submit → /auth/login → /auth/callback → poll-success.
//
// Fakes the IdP redirect by short-circuiting through the broker's own
// /auth/callback with a synthetic code; the fakeVerifier accepts any
// code and returns the configured claims, which mirrors what a real
// IdP redirect would deliver. The point of this test is the cookie +
// state plumbing across handlers, not the OAuth exchange itself.
func TestDeviceFlowIntegrationHappyPath(t *testing.T) {
	verifier := &fakeVerifier{
		authURL:     "https://idp.example.com/authorize",
		claims:      Claims{Email: "alice@example.com", EmailVerified: true, Subject: "google|alice"},
		rawIDToken:  "raw-id-token-abc",
		accessToken: "raw-access-token-xyz",
	}
	srv, _ := newTestServer(t, deviceTestConfig(), verifier, fake.NewSimpleClientset())

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := testClient(srv)
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// Step 1: /device/authorize.
	authResp, err := client.PostForm(srv.URL+"/device/authorize", url.Values{
		"client_id": {"demarkus-cli"},
	})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	var auth deviceAuthorizeResponse
	if err := json.NewDecoder(authResp.Body).Decode(&auth); err != nil {
		t.Fatalf("decode authorize: %v", err)
	}
	_ = authResp.Body.Close()

	// Step 2: first poll → pending.
	if err := expectDeviceTokenError(t, srv, auth.DeviceCode, "authorization_pending"); err != nil {
		t.Fatal(err)
	}

	// Step 3: user_code form submit → device cookie + 302 to /auth/login.
	formResp, err := client.PostForm(srv.URL+"/device", url.Values{"user_code": {auth.UserCode}})
	if err != nil {
		t.Fatalf("form submit: %v", err)
	}
	_ = formResp.Body.Close()
	if formResp.StatusCode != http.StatusFound {
		t.Fatalf("form status = %d, want 302", formResp.StatusCode)
	}

	// Step 4: /auth/login → 302 to IdP authorize URL with state nonce.
	// /auth/login also consumes the device cookie here, pinning the
	// device_code into the signed state and clearing the cookie so a
	// stale value can't route a later browser callback through the
	// device branch.
	loginResp, err := client.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_ = loginResp.Body.Close()
	if cleared := findDeviceCookie(loginResp.Cookies()); cleared == nil || cleared.MaxAge >= 0 {
		t.Errorf("device cookie not cleared at /auth/login: %+v", cleared)
	}
	loc, err := url.Parse(loginResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse login Location: %v", err)
	}
	nonce := loc.Query().Get("state")
	if nonce == "" {
		t.Fatalf("missing state nonce, Location=%q", loginResp.Header.Get("Location"))
	}

	// Step 5: simulate the IdP redirect back to /auth/callback. The
	// jar holds the state cookie (with DeviceCode pinned inside the
	// signed payload); the device cookie has already been consumed at
	// /auth/login. deviceCallback engages on state.DeviceCode and
	// runs the Bind path.
	req, _ := http.NewRequest(http.MethodGet,
		srv.URL+"/auth/callback?code=abc&state="+url.QueryEscape(nonce), http.NoBody)
	cbResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer func() { _ = cbResp.Body.Close() }()
	if cbResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(cbResp.Body)
		t.Fatalf("callback status = %d body=%s", cbResp.StatusCode, body)
	}
	if ct := cbResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("callback content-type = %q, want text/html (device branch should render done page)", ct)
	}
	body, _ := io.ReadAll(cbResp.Body)
	if !strings.Contains(string(body), "Device connected") {
		t.Errorf("done page missing expected text, body=%s", body)
	}

	// Step 6: subsequent poll → success with forwarded tokens. Advance
	// the store's lastPolledAt past the slow_down threshold by using a
	// short pollInterval in the test config (testPollInterval = 5s) —
	// since this test runs in the same goroutine without sleeping, we
	// inspect the store directly instead of polling twice.
	resp, err := testClient(srv).PostForm(srv.URL+"/device/token", url.Values{
		"grant_type":  {deviceGrantType},
		"device_code": {auth.DeviceCode},
	})
	if err != nil {
		t.Fatalf("final poll: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// First poll after Bind landed earlier (Step 2) already set
	// lastPolledAt, so this poll falls inside the slow_down window
	// even though Status==complete. Status takes precedence over
	// SlowDown in Poll dispatch (terminal states short-circuit),
	// so the response must be the 200 success body.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("final poll status = %d body=%s", resp.StatusCode, body)
	}
	var success deviceTokenSuccess
	if err := json.NewDecoder(resp.Body).Decode(&success); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if success.IDToken != "raw-id-token-abc" {
		t.Errorf("id_token = %q, want forwarded value", success.IDToken)
	}
	if success.AccessToken != "raw-access-token-xyz" {
		t.Errorf("access_token = %q, want forwarded value", success.AccessToken)
	}
	// PR4 contract: every device-code completion mints a refresh token.
	// 32 bytes hex = 64 chars; assert hex validity too so a 64-byte
	// garbage string can't masquerade as a real token in a future
	// regression.
	decoded, err := hex.DecodeString(success.RefreshToken)
	if err != nil {
		t.Errorf("refresh_token is not valid hex: %v (raw=%q)", err, success.RefreshToken)
	}
	if len(decoded) != refreshTokenBytes {
		t.Errorf("refresh_token decoded len = %d, want %d", len(decoded), refreshTokenBytes)
	}
}

// TestDeviceCallbackRefreshMintFailureLeavesPending pins PR4's
// "never ship anything broken" posture: when the refresh-token mint
// fails after a successful OAuth exchange, deviceCallback MUST leave
// the device-code grant in statusPending so the polling client retries
// (or eventually sees expired_token). Translating to access_denied or
// hand-rolling a half-bound state would silently leak an IdP-verified
// claim into a state the polling client can't recover from.
func TestDeviceCallbackRefreshMintFailureLeavesPending(t *testing.T) {
	verifier := &fakeVerifier{
		authURL:    "https://idp.example.com/authorize",
		claims:     Claims{Email: "alice@example.com", EmailVerified: true, Subject: "google|alice"},
		rawIDToken: "id",
	}
	srv, broker := newTestServer(t, deviceTestConfig(), verifier, fake.NewSimpleClientset())
	// Force refresh-mint failures: randFn always errors.
	broker.refreshStore.randFn = func(_ []byte) (int, error) {
		return 0, errors.New("simulated refresh mint failure")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := testClient(srv)
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	authResp, err := client.PostForm(srv.URL+"/device/authorize", url.Values{
		"client_id": {"demarkus-cli"},
	})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	var auth deviceAuthorizeResponse
	if err := json.NewDecoder(authResp.Body).Decode(&auth); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = authResp.Body.Close()

	formResp, err := client.PostForm(srv.URL+"/device", url.Values{"user_code": {auth.UserCode}})
	if err != nil {
		t.Fatalf("form: %v", err)
	}
	_ = formResp.Body.Close()

	loginResp, err := client.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_ = loginResp.Body.Close()
	loc, _ := url.Parse(loginResp.Header.Get("Location"))
	nonce := loc.Query().Get("state")

	req, _ := http.NewRequest(http.MethodGet,
		srv.URL+"/auth/callback?code=abc&state="+url.QueryEscape(nonce), http.NoBody)
	cbResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_ = cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, want 200 (done page renders on mint failure too)", cbResp.StatusCode)
	}

	// Grant must still be pending, NOT access_denied (that's reserved
	// for IdP-side denial) and NOT statusComplete (we never minted
	// the refresh token to put in the success body).
	if err := expectDeviceTokenError(t, srv, auth.DeviceCode, "authorization_pending"); err != nil {
		t.Fatal(err)
	}
}

// TestDeviceTokenRefreshGrant exercises the PR4 refresh-grant
// branch end-to-end: Issue a refresh token via deviceCallback, then
// POST /device/token with grant_type=refresh_token and verify the
// response carries a fresh broker-signed id_token whose claims
// match the original device-flow identity.
func TestDeviceTokenRefreshGrant(t *testing.T) {
	verifier := &fakeVerifier{
		authURL:    "https://idp.example.com/authorize",
		claims:     Claims{Email: "alice@example.com", EmailVerified: true, Subject: "google|alice", Groups: []string{"eng"}},
		rawIDToken: "raw-id-token-abc",
	}
	signer := newTestIDTokenSigner(t)
	cfg := deviceTestConfig()
	cfg.Server.IDTokenTTL = 5 * time.Minute
	srv, broker := newTestServerWithSigner(t, cfg, verifier, fake.NewSimpleClientset(), signer)

	// Complete a device flow up to Bind by driving the store
	// directly — the full /auth/callback dance is covered by
	// TestDeviceFlowIntegrationHappyPath.
	deviceCode, _, _, err := broker.deviceStore.Authorize()
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	rawRefresh, err := broker.refreshStore.Issue(context.Background(),
		&verifier.claims, "", broker.cfg.Server.RefreshTokenTTL)
	if err != nil {
		t.Fatalf("refresh Issue: %v", err)
	}
	if err := broker.deviceStore.Bind(deviceCode,
		&ExchangeResult{Claims: verifier.claims, RawIDToken: "raw-id-token-abc"},
		rawRefresh); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// Refresh-grant exchange.
	resp, err := testClient(srv).PostForm(srv.URL+"/device/token", url.Values{
		"grant_type":    {refreshGrantType},
		"refresh_token": {rawRefresh},
	})
	if err != nil {
		t.Fatalf("POST /device/token refresh: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("refresh status = %d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var out deviceTokenSuccess
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.IDToken == "" {
		t.Fatal("id_token empty after refresh")
	}
	if out.IDToken != out.AccessToken {
		t.Errorf("id_token != access_token; PR4 contract is same JWT in both fields")
	}
	// Refresh response returns the SAME refresh_token; PR4 does
	// not rotate (plan §Out of Scope).
	if out.RefreshToken != rawRefresh {
		t.Errorf("refresh_token rotated: got %q, want %q", out.RefreshToken, rawRefresh)
	}
	if out.TokenType != "Bearer" {
		t.Errorf("token_type = %q", out.TokenType)
	}
	if out.ExpiresIn <= 0 || out.ExpiresIn > int((5*time.Minute).Seconds())+1 {
		t.Errorf("expires_in = %d, want ~%d", out.ExpiresIn, int((5 * time.Minute).Seconds()))
	}

	// Verify the broker-signed id_token round-trips through the
	// broker's own signer with the original claims intact.
	gotClaims, err := signer.VerifyIDToken(out.IDToken, cfg.Server.PublicURL, time.Now())
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if gotClaims.Email != verifier.claims.Email {
		t.Errorf("Email = %q, want %q", gotClaims.Email, verifier.claims.Email)
	}
	if gotClaims.Subject != verifier.claims.Subject {
		t.Errorf("Subject = %q", gotClaims.Subject)
	}
	if len(gotClaims.Groups) != 1 || gotClaims.Groups[0] != "eng" {
		t.Errorf("Groups = %v", gotClaims.Groups)
	}
}

func TestDeviceTokenRefreshErrors(t *testing.T) {
	signer := newTestIDTokenSigner(t)
	tests := []struct {
		name string
		form url.Values
		want string
	}{
		{
			"missing refresh_token",
			url.Values{"grant_type": {refreshGrantType}},
			"invalid_request",
		},
		{
			"empty refresh_token",
			url.Values{"grant_type": {refreshGrantType}, "refresh_token": {""}},
			"invalid_request",
		},
		{
			"unknown refresh_token",
			url.Values{"grant_type": {refreshGrantType}, "refresh_token": {strings.Repeat("0", refreshTokenBytes*2)}},
			"invalid_grant",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newTestServerWithSigner(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), signer)
			resp, err := testClient(srv).PostForm(srv.URL+"/device/token", tt.form)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var body deviceTokenError
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Error != tt.want {
				t.Errorf("error = %q, want %q", body.Error, tt.want)
			}
		})
	}
}

func TestDeviceTokenRefreshRejectsRevoked(t *testing.T) {
	// End-to-end: Issue a token, Revoke it, attempt to Refresh.
	// Must return invalid_grant — the same shape as unknown.
	signer := newTestIDTokenSigner(t)
	srv, broker := newTestServerWithSigner(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), signer)
	rawRefresh, err := broker.refreshStore.Issue(context.Background(),
		&Claims{Email: "a@b.com", EmailVerified: true}, "", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := broker.refreshStore.Revoke(context.Background(), rawRefresh); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	resp, err := testClient(srv).PostForm(srv.URL+"/device/token", url.Values{
		"grant_type":    {refreshGrantType},
		"refresh_token": {rawRefresh},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body deviceTokenError
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Error != "invalid_grant" {
		t.Errorf("error = %q, want invalid_grant", body.Error)
	}
}

func TestDeviceTokenRefreshCrossGrantIsolation(t *testing.T) {
	// A refresh_token must NOT be accepted at the device_code
	// branch, and a device_code must NOT be accepted at the
	// refresh branch. The dispatch policy in deviceToken pins this;
	// the test guards against accidental field-coupling regressions.
	signer := newTestIDTokenSigner(t)
	srv, broker := newTestServerWithSigner(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), signer)

	// Issue a refresh token; try it on the device_code branch.
	rawRefresh, err := broker.refreshStore.Issue(context.Background(),
		&Claims{Email: "a@b.com", EmailVerified: true}, "", time.Hour)
	if err != nil {
		t.Fatalf("Issue refresh: %v", err)
	}
	resp1, err := testClient(srv).PostForm(srv.URL+"/device/token", url.Values{
		"grant_type":  {deviceGrantType},
		"device_code": {rawRefresh},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp1.Body.Close() }()
	if resp1.StatusCode != http.StatusBadRequest {
		t.Fatalf("device-branch with refresh_token: status = %d, want 400", resp1.StatusCode)
	}
	// deviceTokenDeviceFlow returns expired_token for unknown
	// device codes (matches RFC 8628 — never leak whether a code
	// existed). A real refresh token sent here looks unknown.
	var err1 deviceTokenError
	_ = json.NewDecoder(resp1.Body).Decode(&err1)
	if err1.Error != "expired_token" {
		t.Errorf("device-branch error = %q, want expired_token", err1.Error)
	}

	// And the reverse: device_code on the refresh branch.
	deviceCode, _, _, err := broker.deviceStore.Authorize()
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	resp2, err := testClient(srv).PostForm(srv.URL+"/device/token", url.Values{
		"grant_type":    {refreshGrantType},
		"refresh_token": {deviceCode},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("refresh-branch with device_code: status = %d, want 400", resp2.StatusCode)
	}
	var err2 deviceTokenError
	_ = json.NewDecoder(resp2.Body).Decode(&err2)
	if err2.Error != "invalid_grant" {
		t.Errorf("refresh-branch error = %q, want invalid_grant", err2.Error)
	}
}

// TestDeviceFlowIntegrationIdPDeny exercises the open-question-1 lean:
// when the IdP redirects /auth/callback with ?error=..., the device
// branch must translate to deviceStore.Deny so a polling client gets
// RFC 8628 access_denied rather than waiting out the device_code TTL.
func TestDeviceFlowIntegrationIdPDeny(t *testing.T) {
	verifier := &fakeVerifier{
		authURL: "https://idp.example.com/authorize",
		claims:  Claims{Email: "alice@example.com", EmailVerified: true, Subject: "google|alice"},
	}
	srv, _ := newTestServer(t, deviceTestConfig(), verifier, fake.NewSimpleClientset())

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := testClient(srv)
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	authResp, err := client.PostForm(srv.URL+"/device/authorize", url.Values{
		"client_id": {"demarkus-cli"},
	})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	var auth deviceAuthorizeResponse
	if err := json.NewDecoder(authResp.Body).Decode(&auth); err != nil {
		t.Fatalf("decode authorize: %v", err)
	}
	_ = authResp.Body.Close()

	formResp, err := client.PostForm(srv.URL+"/device", url.Values{"user_code": {auth.UserCode}})
	if err != nil {
		t.Fatalf("form: %v", err)
	}
	_ = formResp.Body.Close()

	loginResp, err := client.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_ = loginResp.Body.Close()
	loc, _ := url.Parse(loginResp.Header.Get("Location"))
	nonce := loc.Query().Get("state")

	// IdP-error redirect — no `code`, has `error`.
	req, _ := http.NewRequest(http.MethodGet,
		srv.URL+"/auth/callback?error=access_denied&state="+url.QueryEscape(nonce), http.NoBody)
	cbResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_ = cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, want 200 (device done page even on denial)", cbResp.StatusCode)
	}

	if err := expectDeviceTokenError(t, srv, auth.DeviceCode, "access_denied"); err != nil {
		t.Fatal(err)
	}
}

// expectDeviceTokenError POSTs to /device/token and asserts that the
// response body is a deviceTokenError with the given code, 400 status.
// Used to keep TestDeviceTokenStates terse without obscuring failures.
func expectDeviceTokenError(t *testing.T, srv *httptest.Server, deviceCode, want string) error {
	t.Helper()
	resp, err := testClient(srv).PostForm(srv.URL+"/device/token", url.Values{
		"grant_type":  {deviceGrantType},
		"device_code": {deviceCode},
	})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var errBody deviceTokenError
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		return err
	}
	if errBody.Error != want {
		t.Fatalf("error = %q, want %q", errBody.Error, want)
	}
	return nil
}
