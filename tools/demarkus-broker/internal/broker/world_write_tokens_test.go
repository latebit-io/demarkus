package broker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestWorldWriteTokenStoreProvisionIsIdempotent(t *testing.T) {
	cfg := testConfig()
	k8s := fake.NewSimpleClientset()
	store := newWorldWriteTokenStore(cfg, k8s)

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

func TestWorldWriteTokenStoreProvisionUnknownWorld(t *testing.T) {
	cfg := testConfig()
	k8s := fake.NewSimpleClientset()
	store := newWorldWriteTokenStore(cfg, k8s)

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
