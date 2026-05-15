package broker

import (
	"context"
	"encoding/json"
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
		}); err != nil {
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
		if success.TokenType != "Bearer" {
			t.Errorf("token_type = %q, want Bearer", success.TokenType)
		}
	})

	t.Run("unsupported_grant_type", func(t *testing.T) {
		srv, _ := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
		resp, err := testClient(srv).PostForm(srv.URL+"/device/token", url.Values{
			"grant_type":  {"authorization_code"},
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
	srv, _ := newTestServer(t, deviceTestConfig(), verifier, fake.NewSimpleClientset())
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
	var got struct {
		Tokens []MintResult `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Tokens) != 1 {
		t.Fatalf("tokens = %+v, want exactly 1", got.Tokens)
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
	loginResp, err := client.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_ = loginResp.Body.Close()
	loc, err := url.Parse(loginResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse login Location: %v", err)
	}
	nonce := loc.Query().Get("state")
	if nonce == "" {
		t.Fatalf("missing state nonce, Location=%q", loginResp.Header.Get("Location"))
	}

	// Step 5: simulate the IdP redirect back to /auth/callback. The jar
	// still holds the device cookie + state cookie, so deviceCallback
	// engages and runs the Bind path.
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
	// Device cookie should be cleared after the callback.
	if cleared := findDeviceCookie(cbResp.Cookies()); cleared == nil || cleared.MaxAge >= 0 {
		t.Errorf("device cookie not cleared post-callback: %+v", cleared)
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

	authResp, err := client.PostForm(srv.URL+"/device/authorize", url.Values{})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	var auth deviceAuthorizeResponse
	_ = json.NewDecoder(authResp.Body).Decode(&auth)
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
