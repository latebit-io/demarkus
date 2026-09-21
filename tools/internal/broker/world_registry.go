package broker

import (
	"slices"
	"sync"
)

// worldRegistry is the set of worlds a pod serves: the static ones from the
// config file, and the provisioned ones, which are swapped whole from the
// tenant registry. It announces every world that leaves.
type worldRegistry struct {
	static      []WorldConfig // immutable after load
	staticIndex map[string]int

	mu           sync.RWMutex
	dynamic      []WorldConfig
	dynamicIndex map[string]int
	// tenants maps identityKey(issuer, subject) to the pinned slug, so tenant
	// resolution survives an email change.
	tenants map[string]string
	onDrop  []func(name string)
}

func newWorldRegistry(static []WorldConfig) *worldRegistry {
	return &worldRegistry{static: static, staticIndex: indexByName(static)}
}

func indexByName(worlds []WorldConfig) map[string]int {
	index := make(map[string]int, len(worlds))
	for i := range worlds {
		index[worlds[i].Name] = i
	}
	return index
}

// All is a snapshot of static plus provisioned worlds; the slice is the caller's.
func (r *worldRegistry) All() []WorldConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]WorldConfig, 0, len(r.static)+len(r.dynamic))
	return append(append(out, r.static...), r.dynamic...)
}

// Find returns a copy of the named world.
func (r *worldRegistry) Find(name string) (WorldConfig, bool) {
	if i, ok := r.staticIndex[name]; ok {
		return r.static[i], true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if i, ok := r.dynamicIndex[name]; ok {
		return r.dynamic[i], true
	}
	return WorldConfig{}, false
}

// SlugForIdentity resolves a provisioned identity to its pinned world.
func (r *worldRegistry) SlugForIdentity(key string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	slug, ok := r.tenants[key]
	return slug, ok
}

// OnDrop registers a listener for worlds that leave the set. It runs after the
// swap and outside the registry's lock, under whatever lock the caller of
// SetDynamic holds: a listener must not reach back into the provisioner.
func (r *worldRegistry) OnDrop(listener func(name string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onDrop = append(r.onDrop, listener)
}

// SetDynamic replaces the provisioned set; static names win a collision. A
// world is dropped when its name left or now belongs to a newer provisioning.
// It returns the names it refused because no tool URL could address them.
func (r *worldRegistry) SetDynamic(worlds []WorldConfig, tenants map[string]string) (rejected []string) {
	next := make([]WorldConfig, 0, len(worlds))
	for i := range worlds {
		name := worlds[i].Name
		if _, static := r.staticIndex[name]; static {
			continue
		}
		if !worldNameRE.MatchString(name) {
			rejected = append(rejected, name)
			continue
		}
		next = append(next, worlds[i])
	}
	nextIndex := indexByName(next)
	index := make(map[string]string, len(tenants))
	for key, slug := range tenants {
		if _, served := nextIndex[slug]; served {
			index[key] = slug
		}
	}

	r.mu.Lock()
	var dropped []string
	for i := range r.dynamic {
		old := &r.dynamic[i]
		if j, kept := nextIndex[old.Name]; !kept || next[j].generation != old.generation {
			dropped = append(dropped, old.Name)
		}
	}
	r.dynamic, r.dynamicIndex, r.tenants = next, nextIndex, index
	listeners := slices.Clone(r.onDrop)
	r.mu.Unlock()

	for _, name := range dropped {
		for _, listener := range listeners {
			listener(name)
		}
	}
	return rejected
}
