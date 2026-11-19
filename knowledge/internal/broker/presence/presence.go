// Package presence holds who used a gateway lately: one entry per person and
// client, dropped once unseen for the window.
package presence

import (
	"cmp"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// A client names itself, so its name and the count per person are bounded.
	maxAgentLen      = 64
	maxAgentsPerUser = 8
	// maxPeople bounds what peers can add: their word is not verified.
	maxPeople  = 4096
	sweepEvery = time.Minute
	// tellEvery is how often the other replicas hear of someone still here.
	tellEvery = time.Minute
)

// Entry is one person on one client, and when they were last seen.
type Entry struct {
	User  string // verified email
	Agent string // MCP client name as kept, "" for none
	Seen  time.Time
}

// Person is one user's clients, by name, and when any was last seen.
type Person struct {
	User   string
	Agents []string
	Seen   time.Time
}

// Registry is safe for concurrent use.
type Registry struct {
	window time.Duration
	now    func() time.Time

	mu    sync.Mutex
	seen  map[string]map[string]sighting // user, then agent
	swept time.Time
}

// sighting is when an entry was last seen, and when this replica last told
// the others; zero for one it only heard of.
type sighting struct {
	seen, told time.Time
}

// New makes a registry that forgets an entry unseen for window, by now's clock.
func New(window time.Duration, now func() time.Time) *Registry {
	return &Registry{window: window, now: now, seen: map[string]map[string]sighting{}}
}

// Mark records that this replica saw user on agent just now and returns the
// entry as kept; no user is no entry. tell says the other replicas are due
// to hear of it.
func (r *Registry) Mark(user, agent string) (kept Entry, tell bool) {
	now := r.now()
	return r.record(Entry{User: user, Agent: agent, Seen: now}, now, true)
}

// Learn records what another replica saw. Its notes may arrive late and in
// any order: the newest time wins, a future one counts as now, and one the
// window has passed is dropped.
func (r *Registry) Learn(e Entry) {
	r.record(e, r.now(), false)
}

func (r *Registry) record(e Entry, now time.Time, own bool) (kept Entry, tell bool) {
	if e.Seen.After(now) {
		e.Seen = now
	}
	if e.User == "" || now.Sub(e.Seen) >= r.window {
		return Entry{}, false
	}
	e.Agent = clean(e.Agent)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now)
	agents := r.seen[e.User]
	if agents == nil {
		if !own && len(r.seen) >= maxPeople {
			return Entry{}, false
		}
		agents = map[string]sighting{}
		// Cloned: the caller's string may be cut from a request buffer.
		r.seen[strings.Clone(e.User)] = agents
	}
	last, known := agents[e.Agent]
	if !known && len(agents) >= maxAgentsPerUser {
		oldest, at := oldestOf(agents)
		// A late note must not push out a client seen after it.
		if e.Seen.Before(at) {
			return Entry{}, false
		}
		delete(agents, oldest)
	}
	if e.Seen.After(last.seen) {
		last.seen = e.Seen
	}
	// Only its own sightings: a replica repeating what it heard would echo.
	if tell = own && now.Sub(last.told) >= tellEvery; tell {
		last.told = now
	}
	agents[e.Agent] = last
	return e, tell
}

// People is everyone seen within the window, by user, each with their
// clients by name.
func (r *Registry) People() []Person {
	now := r.now()
	r.mu.Lock()
	var out []Person
	for user, agents := range r.seen {
		p := Person{User: user}
		for agent, last := range agents {
			if now.Sub(last.seen) >= r.window {
				continue
			}
			p.Agents = append(p.Agents, agent)
			if last.seen.After(p.Seen) {
				p.Seen = last.seen
			}
		}
		if len(p.Agents) > 0 {
			out = append(out, p)
		}
	}
	r.mu.Unlock()
	for i := range out {
		slices.Sort(out[i].Agents)
	}
	slices.SortFunc(out, func(a, b Person) int { return cmp.Compare(a.User, b.User) })
	return out
}

// sweepLocked drops what the window has passed, at most once per sweepEvery:
// a person who never returns must not stay in the map.
func (r *Registry) sweepLocked(now time.Time) {
	if now.Sub(r.swept) < sweepEvery {
		return
	}
	r.swept = now
	for user, agents := range r.seen {
		for agent, last := range agents {
			if now.Sub(last.seen) >= r.window {
				delete(agents, agent)
			}
		}
		if len(agents) == 0 {
			delete(r.seen, user)
		}
	}
}

func oldestOf(agents map[string]sighting) (oldest string, at time.Time) {
	first := true
	for agent, last := range agents {
		if first || last.seen.Before(at) {
			oldest, at, first = agent, last.seen, false
		}
	}
	return oldest, at
}

// note is an Entry between replicas: one line of JSON, the time in seconds.
type note struct {
	User  string `json:"u"`
	Agent string `json:"a"`
	Seen  int64  `json:"t"`
}

// Note is e as one replica tells it to the others, on one line.
func (e Entry) Note() (string, error) {
	line, err := json.Marshal(note{User: e.User, Agent: e.Agent, Seen: e.Seen.Unix()})
	return string(line), err
}

// ParseNote reads what Note wrote. Learn bounds the rest.
func ParseNote(line string) (Entry, error) {
	var n note
	if err := json.Unmarshal([]byte(line), &n); err != nil {
		return Entry{}, err
	}
	if n.User == "" {
		return Entry{}, errors.New("presence: note names no user")
	}
	return Entry{User: n.User, Agent: n.Agent, Seen: time.Unix(n.Seen, 0)}, nil
}

// clean keeps of a client's self-chosen name what is safe to show other
// people and agents: ASCII letters, digits, spaces and ._/-, at most
// maxAgentLen bytes. The result is a new string, empty for a name of nothing else.
func clean(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < maxAgentLen; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.IndexByte(" ._/-", c) >= 0:
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}
