package core

import (
	"slices"
	"testing"
)

func registryWorld(name, generation string) WorldConfig {
	return WorldConfig{Name: name, Generation: generation}
}

// A world that leaves the set, or comes back as a new one under the same name,
// is announced once, after the swap, so every per-world cache can let go of it.
func TestWorldRegistryAnnouncesDroppedWorlds(t *testing.T) {
	registry := newWorldRegistry([]WorldConfig{{Name: "static"}})
	var dropped []string
	registry.OnDrop(func(name string) {
		// The hook runs outside the registry's lock and sees the new set.
		if _, still := registry.Find(name); still && name != "reborn" {
			t.Errorf("%s announced as dropped while still findable", name)
		}
		dropped = append(dropped, name)
	})

	registry.SetDynamic([]WorldConfig{registryWorld("a", "t1"), registryWorld("reborn", "t1"), registryWorld("kept", "t1")}, nil)
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v after the first set, want none", dropped)
	}
	registry.SetDynamic([]WorldConfig{registryWorld("reborn", "t2"), registryWorld("kept", "t1"), registryWorld("static", "t9")}, nil)
	slices.Sort(dropped)
	if !slices.Equal(dropped, []string{"a", "reborn"}) {
		t.Errorf("dropped = %v, want the world that left and the one that was reprovisioned", dropped)
	}
	// Static names win a collision and are never dropped.
	if w, ok := registry.Find("static"); !ok || w.Generation != "" {
		t.Errorf("static world = %+v, %v", w, ok)
	}
	if got := len(registry.All()); got != 3 {
		t.Errorf("worlds = %d, want static, reborn, kept", got)
	}
}

func TestWorldRegistryIdentityIndex(t *testing.T) {
	registry := newWorldRegistry([]WorldConfig{{Name: "static"}})
	registry.SetDynamic([]WorldConfig{registryWorld("eve-1", "t1")}, map[string]string{"iss|eve": "eve-1", "iss|mallory": "static"})
	if slug, ok := registry.SlugForIdentity("iss|eve"); !ok || slug != "eve-1" {
		t.Errorf("eve = %q, %v", slug, ok)
	}
	if _, ok := registry.SlugForIdentity("iss|mallory"); ok {
		t.Error("an identity was pinned to a static world's name")
	}
}

// A registry record written by hand or by an older build may carry a name no
// tool URL can reach. It is kept out and reported, never served.
func TestWorldRegistryRefusesUnaddressableProvisionedNames(t *testing.T) {
	registry := newWorldRegistry(nil)
	rejected := registry.SetDynamic([]WorldConfig{registryWorld("Eve-1", "t1"), registryWorld("eve-2", "t1")}, map[string]string{"iss|a": "Eve-1", "iss|b": "eve-2"})
	if !slices.Equal(rejected, []string{"Eve-1"}) {
		t.Errorf("rejected = %v", rejected)
	}
	if _, ok := registry.Find("Eve-1"); ok {
		t.Error("an unaddressable world is served")
	}
	if _, ok := registry.SlugForIdentity("iss|a"); ok {
		t.Error("an identity is pinned to an unaddressable world")
	}
	if _, ok := registry.Find("eve-2"); !ok {
		t.Error("the valid world went with it")
	}
}

func TestWorldViewServesOneProfile(t *testing.T) {
	reg := newWorldRegistry([]WorldConfig{
		{Name: "team-a"},
		{Name: "team-b", Profile: ProfileKnowledge},
		{Name: "alice-w", Profile: ProfileMemory},
	})
	reg.SetDynamic([]WorldConfig{{Name: "eve-1", Profile: ProfileMemory}}, map[string]string{"idp|eve": "eve-1"})

	knowledge := reg.View(ProfileKnowledge)
	if got := knowledge.All(); len(got) != 2 || got[0].Name != "team-a" || got[1].Name != "team-b" {
		t.Fatalf("knowledge view = %+v, want team-a (blank profile) and team-b", got)
	}
	if _, ok := knowledge.Find("alice-w"); ok {
		t.Fatal("knowledge view found a memory world")
	}
	if slug, _ := knowledge.SlugForIdentity("idp|eve"); slug != "eve-1" {
		t.Fatalf("SlugForIdentity = %q, want the pinned slug from any view", slug)
	}

	memory := reg.View(ProfileMemory)
	if got := memory.All(); len(got) != 2 || got[0].Name != "alice-w" || got[1].Name != "eve-1" {
		t.Fatalf("memory view = %+v, want alice-w and eve-1", got)
	}
	if _, ok := memory.Find("team-a"); ok {
		t.Fatal("memory view found a knowledge world")
	}
	if slug, ok := memory.SlugForIdentity("idp|eve"); !ok || slug != "eve-1" {
		t.Fatalf("memory view SlugForIdentity = %q, %v; want eve-1", slug, ok)
	}
}
