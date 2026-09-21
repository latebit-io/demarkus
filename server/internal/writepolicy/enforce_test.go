package writepolicy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/filestore"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

const blockUntagged = "# Write Policy\n\nstrictness: block\nrequire_tags: domain\n"

var policyMeta = map[string]string{"tags": "category:governance", "type": "Policy"}

func newStore(t *testing.T) backend.Store {
	t.Helper()
	return filestore.New(protocolstore.New(t.TempDir()), catalog.New())
}

func TestEnforce(t *testing.T) {
	raw := newStore(t)
	if _, err := (backendtest.Direct{Store: raw}).WriteVersion(publishpolicy.DocumentPath, 0, []byte(blockUntagged), policyMeta); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	gated := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true})}

	_, err := gated.WriteVersion("/untagged.md", 0, []byte("# U\n"), nil)
	var policyErr *writepolicy.PolicyError
	if !errors.Is(err, writepolicy.ErrPolicyBlocked) || !errors.Is(err, backend.ErrRejected) || !errors.As(err, &policyErr) {
		t.Fatalf("untagged publish = %v, want a blocked PolicyError", err)
	}
	if _, err := gated.WriteVersion("/tagged.md", 0, []byte("# T\n"), map[string]string{"tags": "domain:x"}); err != nil {
		t.Fatalf("tagged publish: %v", err)
	}
	// APPEND is judged on the merged metadata, so inherited tags still comply.
	if _, err := gated.AppendVersion("/tagged.md", 1, []byte("more\n"), nil); err != nil {
		t.Errorf("append inheriting tags: %v", err)
	}
	if _, err := gated.AppendVersion("/tagged.md", 2, []byte("more\n"), map[string]string{"tags": "other:y"}); !errors.Is(err, writepolicy.ErrPolicyBlocked) {
		t.Errorf("append replacing tags = %v, want blocked", err)
	}
	if _, err := gated.WriteVersion(publishpolicy.DocumentPath, 1, []byte("strictness: nonsense\n"), policyMeta); !errors.Is(err, writepolicy.ErrInvalidPolicy) {
		t.Errorf("unenforceable candidate policy = %v, want ErrInvalidPolicy", err)
	}
	if _, err := gated.Archive(publishpolicy.DocumentPath, true); !errors.Is(err, writepolicy.ErrInvalidPolicy) || !errors.Is(err, backend.ErrRejected) {
		t.Errorf("archiving a required policy = %v, want a rejection", err)
	}
}

func TestEnforceKeepsTheCallersPrecondition(t *testing.T) {
	errMine := errors.New("caller precondition")
	gated := writepolicy.Enforce(newStore(t), writepolicy.Options{})
	req := backend.WriteRequest{
		Path: "/a.md", Content: []byte("# A\n"),
		Precondition: func(context.Context, backend.Reader, storefmt.PreparedWrite) error { return errMine },
	}
	if _, err := gated.Publish(context.Background(), req); !errors.Is(err, errMine) {
		t.Errorf("publish = %v, want the caller's precondition to run first", err)
	}
}

func TestRequireAndValidate(t *testing.T) {
	raw := newStore(t)
	if err := writepolicy.Validate(context.Background(), raw); !errors.Is(err, writepolicy.ErrInvalidPolicy) {
		t.Errorf("Validate without a policy = %v, want ErrInvalidPolicy", err)
	}
	optional := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{})}
	if _, err := optional.WriteVersion("/free.md", 0, []byte("# F\n"), nil); err != nil {
		t.Errorf("world without a policy, not required: %v", err)
	}
	required := backendtest.Direct{Store: writepolicy.Enforce(raw, writepolicy.Options{Require: true})}
	if _, err := required.WriteVersion("/gated.md", 0, []byte("# G\n"), nil); !errors.Is(err, writepolicy.ErrInvalidPolicy) {
		t.Errorf("required but missing policy = %v, want ErrInvalidPolicy", err)
	}
}
