package presence

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/protocol/memtest"
)

func newTestRegistry(window time.Duration) (*Registry, *brokertest.FakeClock) {
	c := brokertest.NewFakeClock(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	return New(window, c.Now), c
}

func TestPeopleHoldsWhoWasSeenWithinTheWindow(t *testing.T) {
	r, c := newTestRegistry(10 * time.Minute)
	start := c.Now()
	r.Mark("bob@example.com", "claude-code")
	r.Mark("alice@example.com", "demarkus-library")
	r.Mark("", "claude-code")
	c.Advance(time.Minute)
	r.Mark("alice@example.com", "claude-code")

	c.Set(start.Add(5 * time.Minute))
	want := []Person{
		{User: "alice@example.com", Agents: []string{"claude-code", "demarkus-library"}, Seen: start.Add(time.Minute)},
		{User: "bob@example.com", Agents: []string{"claude-code"}, Seen: start},
	}
	if got := r.People(); !reflect.DeepEqual(got, want) {
		t.Errorf("People = %+v\nwant     %+v", got, want)
	}

	// Past the window for what was seen at start, before any sweep.
	c.Set(start.Add(10 * time.Minute))
	want = []Person{{User: "alice@example.com", Agents: []string{"claude-code"}, Seen: start.Add(time.Minute)}}
	if got := r.People(); !reflect.DeepEqual(got, want) {
		t.Errorf("People after the window = %+v\nwant %+v", got, want)
	}
}

// What a peer saw joins the list at the newest time heard. Its clock is not
// trusted: nothing lands in the future or past the window.
func TestLearnBoundsWhatAPeerSays(t *testing.T) {
	r, c := newTestRegistry(10 * time.Minute)
	now := c.Now()
	r.Learn(Entry{User: "alice@example.com", Agent: "a", Seen: now.Add(-time.Minute)})
	r.Learn(Entry{User: "alice@example.com", Agent: "a", Seen: now.Add(-2 * time.Minute)})
	r.Learn(Entry{User: "carol@example.com", Agent: "a", Seen: now.Add(time.Hour)})
	r.Learn(Entry{User: "bob@example.com", Agent: "a", Seen: now.Add(-10 * time.Minute)})
	r.Learn(Entry{Agent: "a", Seen: now})
	want := []Person{
		{User: "alice@example.com", Agents: []string{"a"}, Seen: now.Add(-time.Minute)},
		{User: "carol@example.com", Agents: []string{"a"}, Seen: now},
	}
	if got := r.People(); !reflect.DeepEqual(got, want) {
		t.Errorf("People = %+v\nwant     %+v", got, want)
	}
}

// A replica tells the others of its own sighting at once, then once per
// tellEvery while the person stays; what it only heard it never repeats.
func TestMarkSaysWhenToTellThePeers(t *testing.T) {
	r, c := newTestRegistry(10 * time.Minute)
	kept, tell := r.Mark("alice@example.com", "a|b")
	if want := (Entry{User: "alice@example.com", Agent: "ab", Seen: c.Now()}); !tell || kept != want {
		t.Fatalf("first Mark = %+v, %v; want %+v told as kept", kept, tell, want)
	}
	c.Advance(tellEvery - time.Second)
	if _, tell := r.Mark("alice@example.com", "a|b"); tell {
		t.Error("told again within tellEvery")
	}
	c.Advance(time.Second)
	if _, tell := r.Mark("alice@example.com", "a|b"); !tell {
		t.Error("not told again after tellEvery")
	}

	r.Learn(Entry{User: "bob@example.com", Agent: "a", Seen: c.Now()})
	c.Advance(time.Hour)
	r.Learn(Entry{User: "bob@example.com", Agent: "a", Seen: c.Now()})
	// Heard, never told: seeing bob here is this replica's first sighting.
	if _, tell := r.Mark("bob@example.com", "a"); !tell {
		t.Error("a first own sighting of someone a peer saw was not told")
	}
}

func TestNoteRoundTrips(t *testing.T) {
	seen := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	// A peer's note may carry anything, a line break included.
	want := Entry{User: "alice@example.com", Agent: "claude code\n\"x\"", Seen: seen}
	line, err := want.Note()
	if err != nil || strings.ContainsAny(line, "\r\n") {
		t.Fatalf("Note = %q, %v; want one line", line, err)
	}
	if got, err := ParseNote(line); err != nil || !got.Seen.Equal(seen) || got.User != want.User || got.Agent != want.Agent {
		t.Errorf("ParseNote = %+v, %v; want %+v", got, err, want)
	}
	for _, bad := range []string{"", "{", `{"a":"x","t":1}`, `[1]`} {
		if got, err := ParseNote(bad); err == nil {
			t.Errorf("ParseNote(%q) = %+v, want an error", bad, got)
		}
	}
}

// The client picks its own name, and others read it: only plain characters
// are kept, a long name is cut, and a person keeps only their newest few.
func TestAClientNameIsBounded(t *testing.T) {
	for name, want := range map[string]string{
		"claude-code":               "claude-code",
		"Claude Desktop 1.2/beta_3": "Claude Desktop 1.2/beta_3",
		"日本語":                       "",
		strings.Repeat("a", 100):    strings.Repeat("a", maxAgentLen),
	} {
		if got := clean(name); got != want {
			t.Errorf("clean(%q) = %q, want %q", name, got, want)
		}
	}
	if got := clean(" x | y\n# ignore `this` [a](b) "); strings.ContainsAny(got, "|\n#`[]()") || got != strings.TrimSpace(got) {
		t.Errorf("clean kept markup or outer spaces: %q", got)
	}

	r, c := newTestRegistry(10 * time.Minute)
	for n := range maxAgentsPerUser + 3 {
		c.Advance(time.Second)
		r.Mark("alice@example.com", fmt.Sprintf("agent-%02d", n))
	}
	got := r.People()[0].Agents
	if len(got) != maxAgentsPerUser || got[0] != "agent-03" {
		t.Errorf("%d clients starting at %q, want the newest %d starting at agent-03", len(got), got[0], maxAgentsPerUser)
	}
}

// A note that arrives late does not push out a client seen after it.
func TestALateNoteKeepsNewerClients(t *testing.T) {
	r, c := newTestRegistry(10 * time.Minute)
	c.Advance(9 * time.Minute)
	for n := range maxAgentsPerUser {
		r.Mark("alice@example.com", fmt.Sprintf("agent-%02d", n))
	}
	want := r.People()
	r.Learn(Entry{User: "alice@example.com", Agent: "late", Seen: c.Now().Add(-9 * time.Minute)})
	if got := r.People(); !reflect.DeepEqual(got, want) {
		t.Errorf("People after a late note = %+v\nwant %+v", got, want)
	}
}

// Peers are not verified, so they cannot grow the list past maxPeople; a
// person this replica saw itself is always kept.
func TestPeersCannotGrowTheListWithoutBound(t *testing.T) {
	r, c := newTestRegistry(10 * time.Minute)
	for n := range maxPeople + 10 {
		r.Learn(Entry{User: fmt.Sprintf("forged-%06d@example.com", n), Seen: c.Now()})
	}
	if got := len(r.People()); got != maxPeople {
		t.Errorf("%d people after peers named %d, want %d", got, maxPeople+10, maxPeople)
	}
	r.Mark("alice@example.com", "claude-code")
	if got := r.People()[0].User; got != "alice@example.com" {
		t.Errorf("first person = %q, want the one seen here", got)
	}
}

// People who came once and never returned leave nothing behind.
func TestRegistryForgetsWhoLeft(t *testing.T) {
	const warmup, cycles = 2000, 20000
	r, c := newTestRegistry(10 * time.Minute)
	come := func(n int) {
		c.Advance(time.Second)
		r.Mark(fmt.Sprintf("reader-%06d@example.com", n), "claude-code")
	}
	for n := range warmup {
		come(n)
	}
	growth := memtest.Retained(func() {
		for n := warmup; n < warmup+cycles; n++ {
			come(n)
		}
	})
	runtime.KeepAlive(r)
	t.Logf("%d entries, heap grew %d bytes", len(r.seen), growth)
	// One person a second for a 10 minute window, plus a sweep interval.
	if len(r.seen) > 700 || growth > 128*1024 {
		t.Errorf("after %d people came and went: %d entries, heap grew %d bytes", cycles, len(r.seen), growth)
	}
}
