package worldpolicy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/client/memory/worldpolicy"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

func answering(r protocol.Response, err error) func(context.Context, string) (protocol.Response, error) {
	return func(_ context.Context, p string) (protocol.Response, error) {
		if p != publishpolicy.DocumentPath {
			return protocol.Response{}, errors.New("read " + p + ", want the policy document")
		}
		return r, err
	}
}

func TestRead(t *testing.T) {
	if p, err := worldpolicy.Read(t.Context(), answering(protocol.Response{Status: protocol.StatusNotFound}, nil)); p != nil || err != nil {
		t.Errorf("no policy document = %+v, %v; want none and no error", p, err)
	}
	if p, err := worldpolicy.Read(t.Context(), answering(protocol.Response{Status: protocol.StatusOK, Body: "# Policy\n"}, nil)); p == nil || err != nil {
		t.Errorf("a policy document = %+v, %v", p, err)
	}
	// A policy that cannot be read is never read as no policy.
	boom := errors.New("boom")
	var unavailable *worldpolicy.Unavailable
	if _, err := worldpolicy.Read(t.Context(), answering(protocol.Response{}, boom)); !errors.As(err, &unavailable) || !errors.Is(err, boom) {
		t.Errorf("transport failure = %v, want Unavailable wrapping it", err)
	}
	_, err := worldpolicy.Read(t.Context(), answering(protocol.Response{Status: protocol.StatusUnauthorized}, nil))
	if !errors.As(err, &unavailable) || unavailable.Status != protocol.StatusUnauthorized || err.Error() != "fetch "+publishpolicy.DocumentPath+": "+protocol.StatusUnauthorized {
		t.Errorf("refused read = %v", err)
	}
}

func TestEffective(t *testing.T) {
	if worldpolicy.Effective(nil, nil, "/doc.md") != nil {
		t.Error("no policy on either side must stay none")
	}
	floor := &publishpolicy.Policy{RequiredTagAxes: []string{"domain"}}
	if got := worldpolicy.Effective(nil, floor, "/doc.md"); got == nil || len(got.RequiredTagAxes) != 1 {
		t.Errorf("floor alone = %+v, want its axes to bind", got)
	}
}
