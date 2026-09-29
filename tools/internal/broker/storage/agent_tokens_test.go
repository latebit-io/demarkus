package storage

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/tools/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/tools/internal/broker/core"
	"github.com/latebit-io/demarkus/tools/internal/token"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	agentSecret = "team-a-token-values"
	agentKey    = "admin"
	agentLabel  = "agent-team-a"
)

func agentTokensConfig() *core.Config {
	cfg := brokertest.NewConfig()
	cfg.AgentTokens = []core.AgentTokenConfig{{World: "team-a", Secret: agentSecret, Key: agentKey, Paths: []string{"/**"}}}
	return cfg
}

func getSecretKey(t *testing.T, k8s *fake.Clientset, ns, name, key string) []byte {
	t.Helper()
	secret, err := k8s.CoreV1().Secrets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret %s/%s: %v", ns, name, err)
	}
	return secret.Data[key]
}

func worldEntry(t *testing.T, k8s *fake.Clientset, label string) (token.Entry, bool) {
	t.Helper()
	file, err := token.ParseBytes(getSecretKey(t, k8s, "team-a", "team-a-tokens", core.TokensSecretKey))
	if err != nil {
		t.Fatalf("parse world tokens.toml: %v", err)
	}
	entry, ok := file.Tokens[label]
	return entry, ok
}

func reconcile(t *testing.T, cfg *core.Config, k8s *fake.Clientset) {
	t.Helper()
	if err := NewAgentTokens(cfg, NewK8sSecretStore(k8s), slog.New(slog.DiscardHandler)).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func TestAgentTokensIssuesTokenWorldAccepts(t *testing.T) {
	cfg := agentTokensConfig()
	k8s := fake.NewSimpleClientset()
	reconcile(t, cfg, k8s)

	raw := string(getSecretKey(t, k8s, "team-a", agentSecret, agentKey))
	if raw == "" {
		t.Fatal("agent Secret has no raw token")
	}
	entry, ok := worldEntry(t, k8s, agentLabel)
	if !ok {
		t.Fatalf("world tokens.toml has no %q entry", agentLabel)
	}
	if entry.Hash != protocol.HashToken(raw) {
		t.Errorf("world hash %q does not match agent token", entry.Hash)
	}
	if strings.Join(entry.Operations, ",") != "publish" || strings.Join(entry.Paths, ",") != "/**" || entry.Expires != "" {
		t.Errorf("entry = %+v, want publish-only on /** without expiry", entry)
	}
}

// The world must know the hash before the agent can read the raw value.
func TestAgentTokensWritesWorldHashBeforeAgentSecret(t *testing.T) {
	cfg := agentTokensConfig()
	k8s := fake.NewSimpleClientset()
	reconcile(t, cfg, k8s)

	var order []string
	for _, action := range k8s.Actions() {
		if create, ok := action.(k8stesting.CreateAction); ok {
			order = append(order, create.GetObject().(*corev1.Secret).Name)
		}
	}
	want := []string{"demarkus-broker-agent-token-team-a", "team-a-tokens", agentSecret}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("create order = %v, want %v", order, want)
	}
}

func TestAgentTokensConvergesAcrossPods(t *testing.T) {
	cfg := agentTokensConfig()
	k8s := fake.NewSimpleClientset()
	reconcile(t, cfg, k8s)
	first := string(getSecretKey(t, k8s, "team-a", agentSecret, agentKey))

	// A second pod with a cold process reuses the record instead of minting.
	reconcile(t, cfg, k8s)
	if got := string(getSecretKey(t, k8s, "team-a", agentSecret, agentKey)); got != first {
		t.Errorf("second pass rotated the token: %q -> %q", first, got)
	}
	body := string(getSecretKey(t, k8s, "team-a", "team-a-tokens", core.TokensSecretKey))
	if n := strings.Count(body, "[tokens."+agentLabel+"]"); n != 1 {
		t.Errorf("world tokens.toml has %d %q entries, want 1:\n%s", n, agentLabel, body)
	}
}

func TestAgentTokensLeavesUnmanagedSecretAlone(t *testing.T) {
	// A bootstrap Job or an operator minted this token; the broker has no record.
	cfg := agentTokensConfig()
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: agentSecret},
		Data:       map[string][]byte{agentKey: []byte("operator-token")},
	})
	reconcile(t, cfg, k8s)

	if got := string(getSecretKey(t, k8s, "team-a", agentSecret, agentKey)); got != "operator-token" {
		t.Errorf("agent Secret rewritten to %q", got)
	}
	for _, ref := range []core.SecretRef{core.AgentTokenRecordRef(cfg, "team-a"), core.WorldTokensRef(&cfg.Worlds[0])} {
		if _, err := k8s.CoreV1().Secrets(ref.Namespace).Get(context.Background(), ref.Name, metav1.GetOptions{}); err == nil {
			t.Errorf("secret %s created for an unmanaged agent token", ref)
		}
	}
}

func TestAgentTokensRecreatesDeletedAgentSecret(t *testing.T) {
	cfg := agentTokensConfig()
	k8s := fake.NewSimpleClientset()
	reconcile(t, cfg, k8s)
	first := string(getSecretKey(t, k8s, "team-a", agentSecret, agentKey))

	if err := k8s.CoreV1().Secrets("team-a").Delete(context.Background(), agentSecret, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete agent Secret: %v", err)
	}
	reconcile(t, cfg, k8s)
	got := string(getSecretKey(t, k8s, "team-a", agentSecret, agentKey))
	if got != first {
		t.Errorf("recreated token %q, want the recorded %q", got, first)
	}
	if entry, ok := worldEntry(t, k8s, agentLabel); !ok || entry.Hash != protocol.HashToken(got) {
		t.Errorf("world does not accept the recreated token: %+v ok=%v", entry, ok)
	}
}

func TestAgentTokensRestoresResetWorldTokens(t *testing.T) {
	cfg := agentTokensConfig()
	k8s := fake.NewSimpleClientset()
	reconcile(t, cfg, k8s)
	raw := string(getSecretKey(t, k8s, "team-a", agentSecret, agentKey))

	// Stale hash under the label, as after a restore from an old backup.
	stale, err := token.AppendBytes(nil, agentLabel, &token.Entry{Hash: "sha256-stale", Paths: []string{"/**"}, Operations: []string{"publish"}})
	if err != nil {
		t.Fatalf("build stale tokens.toml: %v", err)
	}
	world, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get world Secret: %v", err)
	}
	world.Data[core.TokensSecretKey] = stale
	if _, err := k8s.CoreV1().Secrets("team-a").Update(context.Background(), world, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("reset world Secret: %v", err)
	}

	reconcile(t, cfg, k8s)
	if entry, ok := worldEntry(t, k8s, agentLabel); !ok || entry.Hash != protocol.HashToken(raw) {
		t.Errorf("world entry not restored: %+v ok=%v", entry, ok)
	}
}

func TestAgentTokensOneFailureDoesNotStopOthers(t *testing.T) {
	cfg := agentTokensConfig()
	cfg.Worlds = append(cfg.Worlds, core.WorldConfig{
		Name: "team-b", Namespace: "team-b", TokensSecret: "team-b-tokens",
		DefaultToken: core.TokenScope{Paths: []string{"/b"}},
	})
	cfg.AgentTokens = append([]core.AgentTokenConfig{{World: "team-b", Secret: "team-b-token-values", Key: agentKey, Paths: []string{"/**"}}}, cfg.AgentTokens...)
	k8s := fake.NewSimpleClientset()
	k8s.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "team-b" {
			return true, nil, errors.New("apiserver down")
		}
		return false, nil, nil
	})

	err := NewAgentTokens(cfg, NewK8sSecretStore(k8s), slog.New(slog.DiscardHandler)).Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "agent token team-b") {
		t.Fatalf("Reconcile error = %v, want the team-b failure", err)
	}
	if raw := getSecretKey(t, k8s, "team-a", agentSecret, agentKey); len(raw) == 0 {
		t.Error("team-a was skipped after team-b failed")
	}
}

// hangingStore blocks every call until its context ends, like a stuck API
// request, and records the time left before the first call's deadline.
type hangingStore struct {
	once     sync.Once
	deadline chan time.Duration
}

func (h *hangingStore) Mutate(ctx context.Context, _ core.SecretRef, _ func([]byte) ([]byte, error)) error {
	h.once.Do(func() {
		left := time.Duration(-1)
		if deadline, ok := ctx.Deadline(); ok {
			left = time.Until(deadline)
		}
		h.deadline <- left
	})
	<-ctx.Done()
	return ctx.Err()
}

func (h *hangingStore) Delete(ctx context.Context, _ core.SecretRef) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestAgentTokensPassIsBoundedByHalfInterval(t *testing.T) {
	store := &hangingStore{deadline: make(chan time.Duration, 1)}
	a := NewAgentTokens(agentTokensConfig(), store, slog.New(slog.DiscardHandler))
	a.interval = 400 * time.Millisecond
	done := make(chan struct{})
	go func() {
		a.runOnce(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung store call stalled the pass past its timeout")
	}
	half := a.interval / 2
	if left := <-store.deadline; left <= half/2 || left > half {
		t.Errorf("pass deadline %v away, want about %v (interval/2)", left, half)
	}
}
