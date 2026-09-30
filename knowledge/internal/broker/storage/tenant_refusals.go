package storage

import (
	"sync"
	"time"
)

// refusalTTL is how long a provisioning refusal is repeated from memory. Long
// enough to take a refused identity's calls off the provisioner's lock; a
// registry change clears it early, so an admitted identity never waits it out.
const refusalTTL = time.Minute

// maxRememberedRefusals bounds the memory a flood of refused identities costs.
const maxRememberedRefusals = 4096

// tenantRefusals remembers, per identity, a gate or capacity refusal, which
// holds until the registry changes.
type tenantRefusals struct {
	mu   sync.Mutex
	byID map[string]rememberedRefusal
}

type rememberedRefusal struct {
	err   error
	until time.Time
}

// recent is the refusal still in force for identity, nil when there is none.
func (r *tenantRefusals) recent(identity string, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	refusal, ok := r.byID[identity]
	if !ok || !now.Before(refusal.until) {
		return nil
	}
	return refusal.err
}

func (r *tenantRefusals) remember(identity string, refusal error, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID == nil {
		r.byID = make(map[string]rememberedRefusal)
	}
	if len(r.byID) >= maxRememberedRefusals {
		for id, old := range r.byID {
			if !now.Before(old.until) {
				delete(r.byID, id)
			}
		}
		// Still full of live entries: forget them all, never grow.
		if len(r.byID) >= maxRememberedRefusals {
			clear(r.byID)
		}
	}
	r.byID[identity] = rememberedRefusal{err: refusal, until: now.Add(refusalTTL)}
}

// clear forgets every refusal: the registry changed, so any of them may now
// be admitted.
func (r *tenantRefusals) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.byID)
}
