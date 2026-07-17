package broker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/token"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestWorldWriteTokenStoreProvisionIsIdempotent(t *testing.T) {
	cfg := testConfig()
	k8s := fake.NewSimpleClientset()
	store := newWorldWriteTokenStore(cfg, NewK8sSecretStore(k8s))

	first, err := store.Provision(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("Provision first: %v", err)
	}
	if first == "" {
		t.Fatal("Provision returned empty raw token")
	}

	second, err := store.Provision(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("Provision second: %v", err)
	}
	if first != second {
		t.Errorf("Provision returned different tokens across calls:\n  first  = %q\n  second = %q", first, second)
	}

	// The broker's per-world Secret holds exactly one record under
	// the stable label, with the raw token preserved across calls.
	brokerSecret, err := k8s.CoreV1().Secrets(cfg.Server.BrokerNamespace).Get(context.Background(), worldWriteTokenSecretName("team-a"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get broker write-token Secret: %v", err)
	}
	var record writeTokenRecord
	if err := json.Unmarshal(brokerSecret.Data[worldWriteTokenSecretKey], &record); err != nil {
		t.Fatalf("decode write token record: %v", err)
	}
	if record.RawToken != first {
		t.Errorf("broker-secret raw = %q, want %q", record.RawToken, first)
	}
	if record.Label != worldWriteTokenLabel("team-a") {
		t.Errorf("broker-secret label = %q, want %q", record.Label, worldWriteTokenLabel("team-a"))
	}

	// The world's tokens.toml carries exactly the broker's stable
	// label. Re-provisioning never accumulates extra entries.
	worldSecret, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get world tokens Secret: %v", err)
	}
	body := string(worldSecret.Data[TokensSecretKey])
	wantHeader := "[tokens." + worldWriteTokenLabel("team-a") + "]"
	if !contains(body, wantHeader) {
		t.Errorf("world tokens.toml missing %q\nfull:\n%s", wantHeader, body)
	}
}

func TestWorldWriteTokenStoreProvisionRecoversAcrossStores(t *testing.T) {
	// The load-bearing multi-pod case: pod B starts up after pod A
	// has already provisioned a world. Pod B's local cache is empty
	// but the canonical record lives in the broker Secret; Provision
	// must read it back rather than mint a fresh token (which would
	// rewrite the world's tokens.toml under a new hash and orphan
	// every other pod's cached token).
	cfg := testConfig()
	k8s := fake.NewSimpleClientset()

	podA := newWorldWriteTokenStore(cfg, NewK8sSecretStore(k8s))
	first, err := podA.Provision(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("podA.Provision: %v", err)
	}

	podB := newWorldWriteTokenStore(cfg, NewK8sSecretStore(k8s))
	second, err := podB.Provision(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("podB.Provision: %v", err)
	}
	if first != second {
		t.Errorf("pods see different tokens — broker Secret recovery is broken:\n  podA = %q\n  podB = %q", first, second)
	}

	// The world's tokens.toml carries the broker's stable label
	// exactly once across both pods — re-provision from a cold
	// cache must not append duplicate entries.
	worldSecret, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get world tokens Secret: %v", err)
	}
	body := string(worldSecret.Data[TokensSecretKey])
	header := "[tokens." + worldWriteTokenLabel("team-a") + "]"
	count := 0
	for i := 0; i+len(header) <= len(body); i++ {
		if body[i:i+len(header)] == header {
			count++
		}
	}
	if count != 1 {
		t.Errorf("world tokens.toml has %d entries for %q, want exactly 1\nfull:\n%s", count, header, body)
	}
}

func TestWorldWriteTokenStoreRewritesStaleWorldEntry(t *testing.T) {
	// Recovery scenario: an operator (or a disaster recovery flow)
	// deleted the broker's per-world write-token Secret while the
	// world's tokens.toml retained the old `broker-write-*` entry
	// under the stable label. Without a hash check in syncWorldHash,
	// the next Provision would mint a fresh raw token, see the label
	// already present, treat it as a no-op, and cache a token the
	// world will never authorize — every write would burn the
	// propagation-race budget on a doomed token.
	cfg := testConfig()
	label := worldWriteTokenLabel("team-a")
	stale := token.Entry{
		Hash:       "sha256-" + strings.Repeat("d", 64), // deliberately not what Generate would produce
		Paths:      cfg.Worlds[0].DefaultToken.Paths,
		Operations: []string{"publish"}, // broker hardcodes write-token ops to ["publish"]
	}
	staleBytes, err := token.AppendBytes(nil, label, &stale)
	if err != nil {
		t.Fatalf("seed stale tokens.toml: %v", err)
	}
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-tokens", Namespace: "team-a"},
		Data:       map[string][]byte{TokensSecretKey: staleBytes},
	})

	store := newWorldWriteTokenStore(cfg, NewK8sSecretStore(k8s))
	raw, err := store.Provision(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if raw == "" {
		t.Fatal("Provision returned empty raw token")
	}

	// The world's tokens.toml entry must have been rewritten to
	// match the broker Secret's canonical hash, not silently left
	// pointing at the stale one.
	worldSecret, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get world tokens Secret: %v", err)
	}
	parsed, err := token.ParseBytes(worldSecret.Data[TokensSecretKey])
	if err != nil {
		t.Fatalf("parse world tokens Secret: %v", err)
	}
	got, ok := parsed.Tokens[label]
	if !ok {
		t.Fatalf("world tokens.toml lost %q after rewrite", label)
	}
	if got.Hash == stale.Hash {
		t.Errorf("world tokens.toml still carries stale hash %q after Provision", got.Hash)
	}
	if got.Hash != protocol.HashToken(raw) {
		t.Errorf("world hash %q does not match Provision's raw token hash %q", got.Hash, protocol.HashToken(raw))
	}
}

func TestWorldWriteTokenStoreProvisionIgnoresConfiguredOperations(t *testing.T) {
	// Regression guard: the broker MUST hardcode the write-token
	// operations to ["publish"]. The operations knob was removed from
	// worlds[].defaultToken entirely, so there is no operator config
	// to honor; Provision must still emit exactly ["publish"]. If
	// "read" ever leaked through (e.g. via a well-intentioned "make it
	// configurable" refactor), the server's RequiresReadAuth would
	// flip on for every path matched by this token and the open-bearer
	// read path that the entire broker simplification rests on would
	// silently start returning unauthorized.
	cfg := testConfig()

	k8s := fake.NewSimpleClientset()
	store := newWorldWriteTokenStore(cfg, NewK8sSecretStore(k8s))
	if _, err := store.Provision(context.Background(), "team-a"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	brokerSecret, err := k8s.CoreV1().Secrets(cfg.Server.BrokerNamespace).Get(context.Background(), worldWriteTokenSecretName("team-a"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get broker write-token Secret: %v", err)
	}
	var record writeTokenRecord
	if err := json.Unmarshal(brokerSecret.Data[worldWriteTokenSecretKey], &record); err != nil {
		t.Fatalf("decode write token record: %v", err)
	}
	if len(record.Entry.Operations) != 1 || record.Entry.Operations[0] != "publish" {
		t.Errorf("Entry.Operations = %v, want exactly [\"publish\"] (operator config MUST be ignored)", record.Entry.Operations)
	}
}

func TestWorldWriteTokenStoreProvisionUnknownWorld(t *testing.T) {
	cfg := testConfig()
	k8s := fake.NewSimpleClientset()
	store := newWorldWriteTokenStore(cfg, NewK8sSecretStore(k8s))

	_, err := store.Provision(context.Background(), "no-such-world")
	if err == nil {
		t.Fatal("Provision returned nil error for unknown world, want errWorldNotFound")
	}
	// Federation/graph callers depend on this sentinel to skip
	// outside-org candidates without surfacing a noisy error.
	var notFound *errWorldNotFound
	if !errors.As(err, &notFound) {
		t.Errorf("Provision error = %v (%T), want *errWorldNotFound", err, err)
	}
	// No Secret was created in the broker namespace.
	if _, err := k8s.CoreV1().Secrets(cfg.Server.BrokerNamespace).Get(context.Background(), worldWriteTokenSecretName("no-such-world"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("broker Secret unexpectedly created for unknown world: %v", err)
	}
}

// contains is a small string-contains helper local to this test
// file so the assertion above doesn't pull in strings just for one
// call (mirroring the pattern already used in configwatch tests).
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestProvisionFileBackend pins the single-host seam: Provision against the
// file store mints once, lands the hash in the world's local tokens.toml
// (the file the co-located demarkus-server watches), and re-provision is a
// no-op that does not rewrite the file.
func TestProvisionFileBackend(t *testing.T) {
	dir := t.TempDir()
	tokensFile := filepath.Join(dir, "tokens.toml")
	cfg := &Config{
		Storage: StorageConfig{Backend: storageBackendFile, Dir: filepath.Join(dir, "state")},
		Worlds: []WorldConfig{{
			Name:            "soul",
			TokensFile:      tokensFile,
			InternalAddress: "localhost:6309",
			DefaultToken:    TokenScope{Paths: []string{"/*"}},
		}},
	}
	store := newWorldWriteTokenStore(cfg, NewFileSecretStore())

	raw, err := store.Provision(context.Background(), "soul")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if raw == "" {
		t.Fatal("Provision returned empty token")
	}

	parsed, err := token.ParseBytes(mustRead(t, tokensFile))
	if err != nil {
		t.Fatalf("parse tokens.toml: %v", err)
	}
	entry, ok := parsed.Tokens[worldWriteTokenLabel("soul")]
	if !ok {
		t.Fatalf("tokens.toml missing label %q", worldWriteTokenLabel("soul"))
	}
	if entry.Hash != protocol.HashToken(raw) {
		t.Error("tokens.toml hash does not match minted token")
	}

	// Fresh store (broker restart): converges on the same token, no rewrite.
	before := mustStat(t, tokensFile)
	raw2, err := newWorldWriteTokenStore(cfg, NewFileSecretStore()).Provision(context.Background(), "soul")
	if err != nil {
		t.Fatalf("re-Provision: %v", err)
	}
	if raw2 != raw {
		t.Error("re-provision minted a different token instead of converging")
	}
	if after := mustStat(t, tokensFile); !after.ModTime().Equal(before.ModTime()) {
		t.Error("tokens.toml rewritten on steady-state re-provision")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
