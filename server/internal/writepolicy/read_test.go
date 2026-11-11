package writepolicy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/filestore"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

// noView refuses views: a policy read on a store that reads current
// documents lock-free must never open one, since it runs inside another
// world's commit.
type noView struct{ *filestore.Store }

func (noView) OpenReadView(context.Context) (backend.ReadView, error) {
	return nil, errors.New("a view was opened for a policy read")
}

func TestReadPolicyTakesNoViewOnALockFreeStore(t *testing.T) {
	raw := newStore(t)
	if _, err := (backendtest.Direct{Store: raw}).WriteVersion(publishpolicy.DocumentPath, 0, []byte(warnPolicy), policyMeta); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	policy, err := writepolicy.Read(context.Background(), noView{raw.(*filestore.Store)}, true)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if policy.Strictness != publishpolicy.Warn {
		t.Fatalf("strictness = %v, want warn", policy.Strictness)
	}
	if _, err := writepolicy.Read(context.Background(), noView{newStore(t).(*filestore.Store)}, true); err == nil {
		t.Fatal("missing required policy read as present")
	}
}

// A store behind the gate still reads lock-free: wrapping it must not turn a
// policy read into a view, the deadlock half inside another world's commit.
func TestEnforcedStoreReadsCurrentWithoutAView(t *testing.T) {
	raw := newStore(t)
	if _, err := (backendtest.Direct{Store: raw}).WriteVersion(publishpolicy.DocumentPath, 0, []byte(warnPolicy), policyMeta); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	gated := writepolicy.Enforce(noView{raw.(*filestore.Store)}, writepolicy.Options{})
	policy, err := writepolicy.Read(context.Background(), gated, true)
	if err != nil || policy.Strictness != publishpolicy.Warn {
		t.Fatalf("Read through the gate = %v, %v; want the warn policy", policy.Strictness, err)
	}
	if err := writepolicy.Validate(context.Background(), gated); err != nil {
		t.Errorf("Validate through the gate: %v", err)
	}
}

// viewOnly is a store with no lock-free read: CurrentOf reads it through a
// view of its own.
type viewOnly struct{ backend.Store }

func TestCurrentOfFallsBackToAView(t *testing.T) {
	raw := newStore(t)
	if _, err := (backendtest.Direct{Store: raw}).WriteVersion("/a.md", 0, []byte("# A\n"), nil); err != nil {
		t.Fatal(err)
	}
	doc, err := writepolicy.CurrentOf(viewOnly{raw}).ReadCurrent(context.Background(), "/a.md")
	if err != nil || string(doc.Content) != "# A\n" {
		t.Fatalf("ReadCurrent through a view = %v, %v", doc, err)
	}
	if _, err := writepolicy.CurrentOf(viewOnly{raw}).ReadCurrent(context.Background(), "/none.md"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("missing document through a view = %v, want not found", err)
	}
}
