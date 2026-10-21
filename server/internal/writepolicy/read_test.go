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
