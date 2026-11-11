// Package worldpolicy reads the write policy a store will judge a write by:
// the world's own document, merged over a floor world's where there is one.
package worldpolicy

import (
	"context"
	"fmt"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

// Unavailable is a policy document that could not be read: a failed fetch
// (Err) or a status other than ok or not-found. A caller refuses rather than
// judge without the policy.
type Unavailable struct {
	Status string
	Err    error
}

func (e *Unavailable) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("fetch %s: %s", publishpolicy.DocumentPath, e.Status)
}

func (e *Unavailable) Unwrap() error { return e.Err }

// Read reads one world's policy through fetch; nil when it has none.
func Read(ctx context.Context, fetch func(ctx context.Context, path string) (protocol.Response, error)) (*publishpolicy.Policy, error) {
	r, err := fetch(ctx, publishpolicy.DocumentPath)
	switch {
	case err != nil:
		return nil, &Unavailable{Err: err}
	case r.Status == protocol.StatusNotFound:
		return nil, nil
	case r.Status != protocol.StatusOK:
		return nil, &Unavailable{Status: r.Status}
	}
	policy := publishpolicy.Parse(r.Body)
	return &policy, nil
}

// Effective is the policy a write at path is judged by: own merged over
// floor, each layer's exemptions applied. Nil when neither world has one.
func Effective(own, floor *publishpolicy.Policy, path string) *publishpolicy.Policy {
	if floor == nil && own == nil {
		return nil
	}
	var world publishpolicy.Policy
	if own != nil {
		world = *own
	}
	effective := publishpolicy.Effective(floor, world, path)
	return &effective
}
