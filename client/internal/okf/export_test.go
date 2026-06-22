package okf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestYamlScalar(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"", `""`},
		{"a: b", `"a: b"`},
		{"https://x/y", "https://x/y"},
		{"ends:", `"ends:"`},
		{" lead", `" lead"`},
		{"[bracket", `"[bracket"`},
		{`has"quote`, `has"quote`}, // mid-scalar quote is valid bare YAML
	}
	for _, tt := range tests {
		if got := yamlScalar(tt.in); got != tt.want {
			t.Errorf("yamlScalar(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStripAbsLinks(t *testing.T) {
	tests := []struct{ name, body, pfx, want string }{
		{"strip prefix", "[a](/vendor/x.md)", "vendor", "[a](/x.md)"},
		{"boundary not matched", "[a](/vendorish/x.md)", "vendor", "[a](/vendorish/x.md)"},
		{"relative untouched", "[a](x.md)", "vendor", "[a](x.md)"},
		{"no prefix noop", "[a](/vendor/x.md)", "", "[a](/vendor/x.md)"},
		{"fragment preserved", "[a](/vendor/x.md#s)", "vendor", "[a](/x.md#s)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripAbsLinks(tt.body, tt.pfx); got != tt.want {
				t.Errorf("stripAbsLinks = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRelFromPath(t *testing.T) {
	if got, err := relFromPath("/vendor/tables/o.md", "vendor"); err != nil || got != "tables/o.md" {
		t.Errorf("relFromPath vendor = %q, %v", got, err)
	}
	if got, err := relFromPath("/o.md", ""); err != nil || got != "o.md" {
		t.Errorf("relFromPath root = %q, %v", got, err)
	}
	// A path that climbs out of the bundle root must fail closed.
	if _, err := relFromPath("/vendor/../../etc/passwd.md", "vendor"); err == nil {
		t.Error("expected relFromPath to reject a traversing path")
	}
}

func TestRenderConcept(t *testing.T) {
	meta := map[string]string{
		"title": "Orders", "tags": "a,b", "resource": "https://x", "importance": "0.8",
	}
	got := renderConcept(meta, "# Body\n", "2026-05-28T14:30:00Z")
	for _, want := range []string{
		"type: Document\n", // synthesized
		"title: Orders\n",
		"resource: https://x\n",
		"tags: [a, b]\n",
		"timestamp: 2026-05-28T14:30:00Z\n", // from modified
		"importance: 0.8\n",                 // producer key preserved
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderConcept missing %q in:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "---\n# Body\n") {
		t.Errorf("body not appended after frontmatter:\n%s", got)
	}
}

func TestNormalizeTimestamp(t *testing.T) {
	const mod = "2026-06-22T15:56:55Z"
	tests := []struct{ name, declared, want string }{
		{"rfc3339 passthrough", "2026-05-28T14:30:00Z", "2026-05-28T14:30:00Z"},
		{"date-only expanded", "2026-05-28", "2026-05-28T00:00:00Z"},
		{"offset normalized to UTC", "2026-05-28T10:30:00-04:00", "2026-05-28T14:30:00Z"},
		{"empty falls back to modified", "", mod},
		{"unparseable falls back to modified", "May 28 2026", mod},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeTimestamp(tt.declared, mod); got != tt.want {
				t.Errorf("normalizeTimestamp(%q) = %q, want %q", tt.declared, got, tt.want)
			}
		})
	}
}

func TestRenderConcept_NormalizesTimestamp(t *testing.T) {
	meta := map[string]string{"type": "T", "timestamp": "2026-05-28"}
	got := renderConcept(meta, "# B\n", "2026-06-22T15:56:55Z")
	if !strings.Contains(got, "timestamp: 2026-05-28T00:00:00Z\n") {
		t.Errorf("declared date-only timestamp not normalized:\n%s", got)
	}
}

func TestRenderReserved(t *testing.T) {
	root := renderReserved("index.md", map[string]string{"okf-version": "0.1"}, "# Hub\n")
	if !strings.HasPrefix(root, "---\nokf_version: 0.1\n---\n# Hub\n") {
		t.Errorf("root index okf_version not emitted: %q", root)
	}
	sub := renderReserved("tables/index.md", map[string]string{}, "# Tables\n")
	if sub != "# Tables\n" {
		t.Errorf("non-root reserved file should be body-only, got %q", sub)
	}
}

func TestBuildExport_StripsPrefix(t *testing.T) {
	docs := []ExportDoc{
		{Path: "/vendor/tables/orders.md", Body: "# Orders\n[c](/vendor/tables/customers.md)\n",
			Metadata: map[string]string{"type": "Table", "tags": "sales"}, Modified: "2026-05-28T14:30:00Z"},
	}
	files, err := BuildExport(docs, "/vendor/")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "tables/orders.md" {
		t.Fatalf("path not stripped: %+v", files)
	}
	content := string(files[0].Content)
	if !strings.Contains(content, "tags: [sales]\n") {
		t.Errorf("tags not serialized as list: %s", content)
	}
	if !strings.Contains(content, "[c](/tables/customers.md)") {
		t.Errorf("link prefix not stripped: %s", content)
	}
}

// TestRoundTrip_ImportExportConforms is the payoff: an OKF bundle imported into a
// world (then served back via FETCH) and exported again must produce a bundle
// that passes conformance, with the document bodies preserved.
func TestRoundTrip_ImportExportConforms(t *testing.T) {
	src := map[string]string{
		"index.md":            "---\nokf_version: 0.1\n---\n# Hub\n* [O](/tables/orders.md) - o\n",
		"tables/index.md":     "# Tables\n* [O](orders.md) - o\n",
		"tables/orders.md":    "---\ntype: BigQuery Table\ntitle: Orders\ntags: [sales, revenue]\n---\n# Orders\n[c](/tables/customers.md) [r](customers.md)\n",
		"tables/customers.md": "---\ntype: BigQuery Table\ntitle: Customers\n---\n# Customers\n",
	}
	bundle := writeBundle(t, src)

	items, err := BuildImport(bundle, "/vendor/")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the world: FETCH returns the stored body and metadata verbatim.
	docs := make([]ExportDoc, len(items))
	for i, it := range items {
		docs[i] = ExportDoc{Path: it.Path, Body: it.Body, Metadata: it.Metadata, Modified: "2026-05-28T14:30:00Z"}
	}

	out := t.TempDir()
	files, err := BuildExport(docs, "/vendor/")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		full := filepath.Join(out, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, f.Content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	report, err := ValidateBundle(out)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Conforms() {
		t.Errorf("round-tripped bundle does not conform: %+v", report.Findings)
	}
	if errs, _ := report.Counts(); errs != 0 {
		t.Errorf("expected 0 errors, got findings: %+v", report.Findings)
	}

	// Body fidelity: the exported orders body matches the original (minus its
	// frontmatter), links round-tripped back to bundle-relative form.
	got, err := os.ReadFile(filepath.Join(out, "tables", "orders.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, body, _ := SplitFrontmatter(got)
	wantBody := "# Orders\n[c](/tables/customers.md) [r](customers.md)\n"
	if string(body) != wantBody {
		t.Errorf("body fidelity lost:\n got %q\nwant %q", body, wantBody)
	}
}
