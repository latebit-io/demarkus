package toolvocab

import "testing"

func TestSuffixFindsTheToolBehindAnyServerName(t *testing.T) {
	cases := []struct{ in, name, prefix string }{
		{"mark_fetch", "mark_fetch", ""},
		{"mcp__soul__mark_publish", "mark_publish", "mcp__soul__"},
		{"mcp__bookmark__mark_publish", "mark_publish", "mcp__bookmark__"},
		{"mcp__mark_soul__mark_publish", "mark_publish", "mcp__mark_soul__"},
		{"benchmark_notes_mark_append", "mark_append", "benchmark_notes_"},
		{"mark_soul_mark_graph_publish", "mark_graph_publish", "mark_soul_"},
		{"knowledge_mark_lookup_all", "mark_lookup_all", "knowledge_"},
	}
	for _, c := range cases {
		name, prefix, ok := Suffix(c.in)
		if !ok || name != c.name || prefix != c.prefix {
			t.Errorf("Suffix(%q) = %q, %q, %v; want %q, %q", c.in, name, prefix, ok, c.name, c.prefix)
		}
	}
	for _, in := range []string{"read_file", "bookmark_fetch", "mark_unknown", "remark_publish", ""} {
		if name, _, ok := Suffix(in); ok {
			t.Errorf("Suffix(%q) found %q, want none", in, name)
		}
	}
}

func TestProfiles(t *testing.T) {
	if err := ValidProfile("nope"); err == nil {
		t.Error("an unknown profile was accepted")
	}
	if ValidProfile(ProfileLean) != nil || ValidProfile(ProfileFull) != nil {
		t.Error("a named profile was refused")
	}
	if ProfileIncludes(ProfileLean, "mark_discover") || !ProfileIncludes(ProfileFull, "mark_discover") || !ProfileIncludes(ProfileLean, "mark_archive") {
		t.Error("the lean profile does not hide exactly the advanced tools")
	}
	if !StoreWrite["mark_promote"] || StoreWrite["mark_graph"] || StoreWrite["mark_host_setup"] {
		t.Error("the store-write set is not the document writes")
	}
}
