package okf

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func TestBuildImport_RejectsSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.md"), []byte("---\ntype: Doc\n---\n# Real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A .md symlink pointing at content outside the bundle must be refused, not
	// followed and published.
	target := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(target, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link.md")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if _, err := BuildImport(root, ""); err == nil {
		t.Error("expected BuildImport to reject a bundle containing a symlink")
	}
}

func TestSanitizeKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"producer_field", "producer-field"},
		{"DataSteward", "datasteward"},
		{"a..b", "a-b"},
		{"--x--", "x"},
		{"123", "123"},
		{"___", ""},
	}
	for _, tt := range tests {
		if got := sanitizeKey(tt.in); got != tt.want {
			t.Errorf("sanitizeKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMapMetadata(t *testing.T) {
	in := map[string]string{
		"type":         "Table",
		"title":        "Orders",
		"tags":         "a,b",
		"okf_version":  "0.1",  // bundle-level, dropped
		"version":      "9",    // reserved, dropped
		"producer_key": "v",    // sanitized
		"note":         "x\ny", // newline flattened
	}
	meta, warns := mapMetadata(in)

	want := map[string]string{
		"type": "Table", "title": "Orders", "tags": "a,b",
		"producer-key": "v", "note": "x y",
	}
	if !reflect.DeepEqual(meta, want) {
		t.Errorf("mapMetadata = %v, want %v", meta, want)
	}
	joined := strings.Join(warns, "\n")
	for _, sub := range []string{`dropped metadata key "version"`, `sanitized metadata key "producer_key"`, "flattened newlines"} {
		if !strings.Contains(joined, sub) {
			t.Errorf("warnings missing %q; got %v", sub, warns)
		}
	}
	if strings.Contains(joined, "okf_version") {
		t.Errorf("okf_version should be dropped silently, got warn: %v", warns)
	}
}

func TestEnforceCaps_KeyCount(t *testing.T) {
	meta := map[string]string{"type": "T", "title": "X"}
	for i := range protocol.MaxMetaKeys + 10 {
		meta["p"+string(rune('a'+i%26))+string(rune('0'+i/26))] = "v"
	}
	meta, warns := enforceCaps(meta)
	if len(meta) > protocol.MaxMetaKeys {
		t.Fatalf("len(meta) = %d, want <= %d", len(meta), protocol.MaxMetaKeys)
	}
	if meta["type"] != "T" || meta["title"] != "X" {
		t.Errorf("recognized fields should survive cap dropping, got %v", meta)
	}
	if len(warns) == 0 {
		t.Error("expected drop warnings")
	}
}

func TestEnforceCaps_ByteSize(t *testing.T) {
	meta := map[string]string{
		"type":        "Table",
		"description": strings.Repeat("x", protocol.MaxMetaBytes+50),
	}
	meta, warns := enforceCaps(meta)
	if metaSize(meta) > protocol.MaxMetaBytes {
		t.Fatalf("metaSize = %d, want <= %d", metaSize(meta), protocol.MaxMetaBytes)
	}
	if meta["type"] != "Table" {
		t.Errorf("type (higher priority) should survive, got %v", meta)
	}
	if _, ok := meta["description"]; ok {
		t.Errorf("oversized description should be dropped, got %v", meta)
	}
	if len(warns) == 0 {
		t.Error("expected a drop warning")
	}
}

func TestRewriteAbsLinks(t *testing.T) {
	tests := []struct {
		name, body, pfx, want string
	}{
		{"no prefix is noop", "[a](/x.md)", "", "[a](/x.md)"},
		{"absolute rewritten", "see [a](/tables/x.md) now", "vendor", "see [a](/vendor/tables/x.md) now"},
		{"relative untouched", "[a](x.md)", "vendor", "[a](x.md)"},
		{"external untouched", "[a](https://e/x.md)", "vendor", "[a](https://e/x.md)"},
		{"fragment preserved", "[a](/x.md#sec)", "vendor", "[a](/vendor/x.md#sec)"},
		{"multiple", "[a](/a.md) [b](/b.md)", "p", "[a](/p/a.md) [b](/p/b.md)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rewriteAbsLinks(tt.body, tt.pfx); got != tt.want {
				t.Errorf("rewriteAbsLinks = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJoinPrefix(t *testing.T) {
	if got := joinPrefix("", "tables/orders.md"); got != "/tables/orders.md" {
		t.Errorf("joinPrefix empty = %q", got)
	}
	if got := joinPrefix("vendor", "tables/orders.md"); got != "/vendor/tables/orders.md" {
		t.Errorf("joinPrefix vendor = %q", got)
	}
}

func TestBuildImport_Integration(t *testing.T) {
	root := writeBundle(t, map[string]string{
		"index.md":         "---\nokf_version: 0.1\n---\n# Hub\n* [O](/tables/orders.md) - o\n",
		"tables/orders.md": "---\ntype: BigQuery Table\ntitle: Orders\ntags: [sales, revenue]\n---\n# Orders\n[cust](/tables/customers.md) and [rel](customers.md)\n",
	})
	items, err := BuildImport(root, "/vendor/")
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]PublishItem{}
	for _, it := range items {
		byPath[it.Path] = it
	}

	idx, ok := byPath["/vendor/index.md"]
	if !ok {
		t.Fatalf("missing index item; paths: %v", keysOf(byPath))
	}
	if idx.Metadata["okf-version"] != "0.1" {
		t.Errorf("index okf-version = %q, want 0.1", idx.Metadata["okf-version"])
	}
	if strings.Contains(idx.Body, "okf_version") || strings.HasPrefix(idx.Body, "---") {
		t.Errorf("root index frontmatter should be stripped from body: %q", idx.Body)
	}
	if !strings.Contains(idx.Body, "/vendor/tables/orders.md") {
		t.Errorf("index absolute link not rewritten: %q", idx.Body)
	}

	ord, ok := byPath["/vendor/tables/orders.md"]
	if !ok {
		t.Fatalf("missing orders item; paths: %v", keysOf(byPath))
	}
	if ord.Metadata["type"] != "BigQuery Table" || ord.Metadata["tags"] != "sales,revenue" {
		t.Errorf("orders metadata = %v", ord.Metadata)
	}
	if !strings.Contains(ord.Body, "/vendor/tables/customers.md") {
		t.Errorf("absolute link not rewritten: %q", ord.Body)
	}
	if !strings.Contains(ord.Body, "[rel](customers.md)") {
		t.Errorf("relative link should be untouched: %q", ord.Body)
	}
}

func keysOf(m map[string]PublishItem) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
