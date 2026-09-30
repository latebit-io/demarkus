package core

import (
	"slices"
	"strings"
	"sync"
)

// WorldRegistry is the set of worlds a pod serves: the static ones from the
// config file, and the provisioned ones, which are swapped whole from the
// tenant registry. It announces every world that leaves.
type WorldRegistry struct {
	static      []WorldConfig // immutable after load
	staticIndex map[string]int

	mu           sync.RWMutex
	dynamic      []WorldConfig
	dynamicIndex map[string]int
	// tenants maps IdentityKey(issuer, subject) to the pinned slug, so tenant
	// resolution survives an email change.
	tenants map[string]string
	onDrop  []func(name string)
}

func newWorldRegistry(static []WorldConfig) *WorldRegistry {
	defaultProfiles(static)
	return &WorldRegistry{static: static, staticIndex: indexByName(static)}
}

// defaultProfiles settles a blank profile as knowledge at registry ingress,
// the config default, so every lookup compares profiles by equality.
func defaultProfiles(worlds []WorldConfig) {
	for i := range worlds {
		if worlds[i].Profile == "" {
			worlds[i].Profile = ProfileKnowledge
		}
	}
}

// allOf is a snapshot of one profile's worlds, sized once under the lock.
func (r *WorldRegistry) allOf(profile string) []WorldConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []WorldConfig
	for _, list := range [][]WorldConfig{r.static, r.dynamic} {
		for i := range list {
			if list[i].Profile == profile {
				out = append(out, list[i])
			}
		}
	}
	return out
}

func indexByName(worlds []WorldConfig) map[string]int {
	index := make(map[string]int, len(worlds))
	for i := range worlds {
		index[worlds[i].Name] = i
	}
	return index
}

// All is a snapshot of static plus provisioned worlds; the slice is the caller's.
func (r *WorldRegistry) All() []WorldConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]WorldConfig, 0, len(r.static)+len(r.dynamic))
	return append(append(out, r.static...), r.dynamic...)
}

// View is the registry as one gateway sees it: the worlds of its profile.
func (r *WorldRegistry) View(profile string) *WorldView {
	return &WorldView{reg: r, profile: profile}
}

// WorldView is a gateway's window on the shared registry. Names are unique
// across profiles, so a lookup that lands in the other profile is a miss.
type WorldView struct {
	reg     *WorldRegistry
	profile string
}

// All is a snapshot of the profile's worlds; the slice is the caller's.
func (v *WorldView) All() []WorldConfig { return v.reg.allOf(v.profile) }

// Find returns a copy of the named world when it belongs to the profile.
func (v *WorldView) Find(name string) (WorldConfig, bool) {
	w, ok := v.reg.Find(name)
	if !ok || w.Profile != v.profile {
		return WorldConfig{}, false
	}
	return w, true
}

// SlugForIdentity resolves a provisioned identity to its pinned slug; Find
// then decides whether the profile serves it.
func (v *WorldView) SlugForIdentity(key string) (string, bool) { return v.reg.SlugForIdentity(key) }

// OnDrop registers a listener for worlds that leave the registry, any profile.
func (v *WorldView) OnDrop(listener func(name string)) { v.reg.OnDrop(listener) }

// Find returns a copy of the named world.
func (r *WorldRegistry) Find(name string) (WorldConfig, bool) {
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

// LocalName is the name of the local world whose authority is authority. A
// scan without copies; the bearer listener runs it once per connection.
func (r *WorldRegistry) LocalName(authority string) (string, bool) {
	if name, ok := localNamed(r.static, authority); ok {
		return name, true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return localNamed(r.dynamic, authority)
}

func localNamed(worlds []WorldConfig, authority string) (string, bool) {
	for i := range worlds {
		if w := &worlds[i]; w.Local && strings.EqualFold(w.Authority(), authority) {
			return w.Name, true
		}
	}
	return "", false
}

// SlugForIdentity resolves a provisioned identity to its pinned world.
func (r *WorldRegistry) SlugForIdentity(key string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	slug, ok := r.tenants[key]
	return slug, ok
}

// OnDrop registers a listener for worlds that leave the set. It runs after the
// swap and outside the registry's lock, under whatever lock the caller of
// SetDynamic holds: a listener must not reach back into the provisioner.
func (r *WorldRegistry) OnDrop(listener func(name string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onDrop = append(r.onDrop, listener)
}

// SetDynamic replaces the provisioned set; static names win a collision. A
// world is dropped when its name left or now belongs to a newer provisioning.
// It returns the names it refused because no tool URL could address them.
func (r *WorldRegistry) SetDynamic(worlds []WorldConfig, tenants map[string]string) (rejected []string) {
	defaultProfiles(worlds)
	next := make([]WorldConfig, 0, len(worlds))
	for i := range worlds {
		name := worlds[i].Name
		if _, static := r.staticIndex[name]; static {
			continue
		}
		if !WorldNameRE.MatchString(name) {
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
		if j, kept := nextIndex[old.Name]; !kept || next[j].Generation != old.Generation {
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
