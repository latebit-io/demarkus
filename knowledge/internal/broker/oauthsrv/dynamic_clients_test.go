package oauthsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/storage"
	"github.com/latebit-io/demarkus/server/blob"
	"k8s.io/client-go/kubernetes/fake"
)

// Register-then-authorize: the MCP-host path. A dynamically registered
// https redirect is trusted; an unregistered one stays refused.
func TestAuthorizeTrustsDynamicallyRegisteredRedirect(t *testing.T) {
	srv, _ := newTestServer(t, brokertest.NewConfig(), &brokertest.FakeVerifier{AuthURL: "https://idp.example.com/authorize"}, fake.NewSimpleClientset())
	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	const callback = "https://claude.ai/api/mcp/auth_callback"
	reg, err := client.Post(srv.URL+"/register", "application/json",
		strings.NewReader(`{"client_name":"Claude","redirect_uris":["`+callback+`"]}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer func() { _ = reg.Body.Close() }()
	if reg.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201", reg.StatusCode)
	}
	var regDoc struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(reg.Body).Decode(&regDoc); err != nil {
		t.Fatalf("decode register response: %v", err)
	}

	q := authorizeQuery()
	q.Set("client_id", regDoc.ClientID)
	q.Set("redirect_uri", callback)
	resp, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize with registered https redirect = %d, want 302 to IdP", resp.StatusCode)
	}

	// Same redirect under an UNREGISTERED client_id stays refused.
	q.Set("client_id", "not-registered")
	resp2, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize unregistered: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("authorize with unregistered client = %d, want 400", resp2.StatusCode)
	}
	// A registered client presenting a DIFFERENT redirect is refused.
	q.Set("client_id", regDoc.ClientID)
	q.Set("redirect_uri", "https://attacker.example/steal")
	resp3, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize wrong redirect: %v", err)
	}
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Errorf("authorize with unregistered redirect = %d, want 400", resp3.StatusCode)
	}
}

func TestRegisterRejectsInvalidRedirectURIs(t *testing.T) {
	srv, _ := newTestServer(t, brokertest.NewConfig(), &brokertest.FakeVerifier{}, fake.NewSimpleClientset())
	client := testClient(srv)
	for _, body := range []string{
		`{"redirect_uris":[]}`,
		`{"redirect_uris":["http://attacker.example/cb"]}`,
		`{"redirect_uris":["https://user:pw@host.example/cb"]}`,
		`{"redirect_uris":["https://host.example/cb#frag"]}`,
		`{"redirect_uris":[42]}`,
		`{"redirect_uris":["cursor://evil.example/oauth/callback"]}`,
		`{"redirect_uris":["windsurf://anysphere.cursor-mcp/oauth/callback"]}`,
		`{"redirect_uris":["https://host.example/cb","cursor://evil.example/oauth/callback"]}`,
	} {
		resp, err := client.Post(srv.URL+"/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("register %s: %v", body, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("register %s = %d, want 400", body, resp.StatusCode)
		}
	}
}

// Cursor registers a private-use scheme callback (RFC 8252 §7.1).
// The exact allowlisted URI registers, is trusted at authorize, and
// is replayed by /auth/callback with the authorization code.
func TestAuthorizeTrustsAllowlistedNativeSchemeRedirect(t *testing.T) {
	verifier := &brokertest.FakeVerifier{
		AuthURL: "https://idp.example.com/authorize",
		Claims:  core.Claims{Subject: "google|123", Email: "alice@example.com", EmailVerified: true},
	}
	cfg := brokertest.NewConfig()
	cfg.Server.PublicURL = "https://broker.example.com"
	srv, _ := newTestServer(t, cfg, verifier, fake.NewSimpleClientset())
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := testClient(srv)
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	const callback = "cursor://anysphere.cursor-mcp/oauth/callback"
	reg, err := client.Post(srv.URL+"/register", "application/json",
		strings.NewReader(`{"client_name":"Cursor","redirect_uris":["`+callback+`"]}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer func() {
		if err := reg.Body.Close(); err != nil {
			t.Errorf("close register body: %v", err)
		}
	}()
	if reg.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201", reg.StatusCode)
	}
	var regDoc struct {
		ClientID     string   `json:"client_id"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(reg.Body).Decode(&regDoc); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if len(regDoc.RedirectURIs) != 1 || regDoc.RedirectURIs[0] != callback {
		t.Fatalf("redirect_uris echoed = %v, want [%s]", regDoc.RedirectURIs, callback)
	}

	q := authorizeQuery()
	q.Set("client_id", regDoc.ClientID)
	q.Set("redirect_uri", callback)
	authResp, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if err := authResp.Body.Close(); err != nil {
		t.Errorf("close authorize body: %v", err)
	}
	if authResp.StatusCode != http.StatusFound {
		t.Fatalf("authorize with allowlisted native redirect = %d, want 302 to IdP", authResp.StatusCode)
	}
	idpRedirect, err := url.Parse(authResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse IdP redirect: %v", err)
	}
	if !strings.HasPrefix(idpRedirect.String(), "https://idp.example.com/authorize") {
		t.Fatalf("redirected to %q, want IdP authorize URL", idpRedirect)
	}
	nonce := idpRedirect.Query().Get("state")
	if nonce == "" {
		t.Fatal("missing IdP state nonce")
	}

	// The IdP returns; the jar replays the state cookie and the
	// broker must 302 to the native callback with the code.
	cbReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/auth/callback?code=idp-code-abc&state="+url.QueryEscape(nonce), http.NoBody)
	if err != nil {
		t.Fatalf("build callback request: %v", err)
	}
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer func() {
		if err := cbResp.Body.Close(); err != nil {
			t.Errorf("close callback body: %v", err)
		}
	}()
	if cbResp.StatusCode != http.StatusFound {
		body, readErr := io.ReadAll(cbResp.Body)
		if readErr != nil {
			t.Errorf("read callback body: %v", readErr)
		}
		t.Fatalf("callback status = %d, want 302; body=%s", cbResp.StatusCode, body)
	}
	clientRedirect, err := url.Parse(cbResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse client redirect: %v", err)
	}
	if clientRedirect.Scheme != "cursor" || clientRedirect.Host != "anysphere.cursor-mcp" || clientRedirect.Path != "/oauth/callback" {
		t.Errorf("client redirect = %q, want %s", clientRedirect, callback)
	}
	cq := clientRedirect.Query()
	if cq.Get("code") == "" {
		t.Error("missing code on client redirect")
	}
	if got := cq.Get("state"); got != "client-state-nonce" {
		t.Errorf("state passthrough mismatch: got %q", got)
	}
}

// Per-registration shape bounds keep the count and byte caps honest.
func TestRegisterRejectsOversizedMetadata(t *testing.T) {
	srv, _ := newTestServer(t, brokertest.NewConfig(), &brokertest.FakeVerifier{}, fake.NewSimpleClientset())
	client := testClient(srv)

	manyURIs := make([]string, maxRedirectURIsPerClient+1)
	for i := range manyURIs {
		manyURIs[i] = fmt.Sprintf(`"https://host.example/cb%d"`, i)
	}
	longURI := "https://host.example/" + strings.Repeat("a", maxRedirectURILen)
	longName := strings.Repeat("n", maxClientNameLen+1)
	for _, body := range []string{
		`{"redirect_uris":[` + strings.Join(manyURIs, ",") + `]}`,
		`{"redirect_uris":["` + longURI + `"]}`,
		`{"client_name":"` + longName + `","redirect_uris":["https://host.example/cb"]}`,
	} {
		resp, err := client.Post(srv.URL+"/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("oversized registration = %d, want 400", resp.StatusCode)
		}
	}
}

func newClientStoreForTest(t *testing.T) (*DynamicClientStore, *testState) {
	t.Helper()
	state := newTestState(t)
	store := NewDynamicClientStore(state)
	store.clock = func() time.Time { return time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC) }
	return store, state
}

func TestDynamicClientStoreRegisterLookupSweep(t *testing.T) {
	store, state := newClientStoreForTest(t)
	ctx := context.Background()
	uris := []string{"https://host.example/callback"}
	if err := store.Register(ctx, "client-a", uris, "Host"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := store.Register(ctx, "client-a", uris, "Host"); err == nil {
		t.Fatal("re-registering a held client_id succeeded, want an error")
	}
	if err := store.Register(ctx, "../refresh/x", uris, ""); err == nil {
		t.Fatal("Register with a malformed client_id succeeded")
	}
	record, found, err := store.Lookup(ctx, "client-a")
	if err != nil || !found || !record.allowsRedirect(uris[0]) || record.ClientName != "Host" {
		t.Fatalf("Lookup = %+v, %v, %v", record, found, err)
	}
	for _, id := range []string{"client-b", "../refresh/x", ""} {
		if _, found, err := store.Lookup(ctx, id); err != nil || found {
			t.Errorf("Lookup(%q) = %v, %v; want not found", id, found, err)
		}
	}

	if n, err := store.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("Sweep of a live registration = %d, %v; want 0", n, err)
	}
	store.clock = func() time.Time {
		return time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC).Add(dynamicClientTTL + time.Hour)
	}
	if _, found, _ := store.Lookup(ctx, "client-a"); found {
		t.Error("expired registration still found")
	}
	if n, err := store.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("Sweep of an expired registration = %d, %v; want 1", n, err)
	}
	if _, err := state.Head(ctx, clientsPrefix+"client-a"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("swept registration: %v, want ErrNotFound", err)
	}
}

// The pre-bucket Secret is imported once: live, well-formed entries land in
// the bucket, the Secret is emptied, and a rerun changes nothing.
func TestDynamicClientStoreImportSecret(t *testing.T) {
	store, _ := newClientStoreForTest(t)
	ctx := context.Background()
	cfg := brokertest.NewConfig()
	cfg.Server.DynamicClientsSecret = "dyn-clients"
	ref := core.DynamicClientsRef(cfg)
	secrets := storage.NewK8sSecretStore(fake.NewSimpleClientset())

	if n, err := store.ImportSecret(ctx, secrets, ref); err != nil || n != 0 {
		t.Fatalf("import with no Secret = %d, %v; want 0", n, err)
	}
	if value, err := core.ReadSecret(ctx, secrets, ref); err != nil || value != nil {
		t.Fatalf("import materialized the Secret: %q, %v", value, err)
	}

	now := store.clock()
	legacy, err := json.Marshal(map[string]dynamicClientRecord{
		"live":         {RedirectURIs: []string{"https://host.example/cb"}, Created: now.Add(-time.Hour)},
		"expired":      {RedirectURIs: []string{"https://host.example/cb"}, Created: now.Add(-dynamicClientTTL - time.Hour)},
		"../refresh/x": {RedirectURIs: []string{"https://host.example/cb"}, Created: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := secrets.Mutate(ctx, ref, func([]byte) ([]byte, error) { return legacy, nil }); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n, err := store.ImportSecret(ctx, secrets, ref); err != nil || n != 1 {
		t.Fatalf("import = %d, %v; want 1", n, err)
	}
	if _, found, err := store.Lookup(ctx, "live"); err != nil || !found {
		t.Fatalf("imported registration: %v, %v", found, err)
	}
	for _, id := range []string{"expired", "../refresh/x"} {
		if _, found, _ := store.Lookup(ctx, id); found {
			t.Errorf("%q imported", id)
		}
	}
	if value, err := core.ReadSecret(ctx, secrets, ref); err != nil || len(value) != 0 {
		t.Fatalf("Secret after import = %q, %v; want empty", value, err)
	}
	if n, err := store.ImportSecret(ctx, secrets, ref); err != nil || n != 0 {
		t.Fatalf("rerun = %d, %v; want 0", n, err)
	}
}

// A registration already in the bucket is kept as is; an interrupted import
// reruns without failing on what it already copied.
func TestDynamicClientStoreImportIsIdempotent(t *testing.T) {
	store, _ := newClientStoreForTest(t)
	ctx := context.Background()
	cfg := brokertest.NewConfig()
	ref := core.DynamicClientsRef(cfg)
	secrets := storage.NewK8sSecretStore(fake.NewSimpleClientset())
	if err := store.Register(ctx, "live", []string{"https://host.example/new"}, ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	legacy, err := json.Marshal(map[string]dynamicClientRecord{"live": {RedirectURIs: []string{"https://host.example/old"}, Created: store.clock()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := secrets.Mutate(ctx, ref, func([]byte) ([]byte, error) { return legacy, nil }); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.ImportSecret(ctx, secrets, ref); err != nil {
		t.Fatalf("import: %v", err)
	}
	if record, _, _ := store.Lookup(ctx, "live"); !record.allowsRedirect("https://host.example/new") {
		t.Fatalf("import overwrote the bucket record: %+v", record)
	}
}
