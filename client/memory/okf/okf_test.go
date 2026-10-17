package okf

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSplitFrontmatter(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		wantFM   string
		wantBody string
		wantOK   bool
	}{
		{"no frontmatter", "# Title\nbody\n", "", "# Title\nbody\n", false},
		{"first line not delim", "text\n---\nx\n---\n", "", "text\n---\nx\n---\n", false},
		{"basic", "---\ntype: Table\n---\n# Body\n", "type: Table\n", "# Body\n", true},
		{"crlf", "---\r\ntype: Table\r\n---\r\n# Body\r\n", "type: Table\r\n", "# Body\r\n", true},
		{"closing delim at eof", "---\ntype: Table\n---", "type: Table\n", "", true},
		{"unterminated", "---\ntype: Table\n# Body\n", "", "---\ntype: Table\n# Body\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm, body, ok := SplitFrontmatter([]byte(tt.data))
			if ok != tt.wantOK || fm != tt.wantFM || string(body) != tt.wantBody {
				t.Errorf("SplitFrontmatter = (%q, %q, %v), want (%q, %q, %v)",
					fm, body, ok, tt.wantFM, tt.wantBody, tt.wantOK)
			}
		})
	}
}

func TestParseFrontmatter(t *testing.T) {
	tests := []struct {
		name    string
		fm      string
		want    map[string]string
		wantErr bool
	}{
		{"scalars", "type: Table\ntitle: Orders\n", map[string]string{"type": "Table", "title": "Orders"}, false},
		{"flow list", "tags: [sales, revenue]\n", map[string]string{"tags": "sales,revenue"}, false},
		{"block list", "tags:\n  - sales\n  - revenue\n", map[string]string{"tags": "sales,revenue"}, false},
		{"quoted", `title: "Orders, Q1"`, map[string]string{"title": "Orders, Q1"}, false},
		{"value with colon", "resource: https://x/y:8080\n", map[string]string{"resource": "https://x/y:8080"}, false},
		{"comment and blank", "# c\n\ntype: Table\n", map[string]string{"type": "Table"}, false},
		{"unparseable line", "type: Table\ngarbage line\n", nil, true},
		{"orphan list item", "  - sales\n", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseFrontmatter(tt.fm)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseFrontmatter = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConceptIDAndReserved(t *testing.T) {
	if got := ConceptID("tables/orders.md"); got != "tables/orders" {
		t.Errorf("ConceptID = %q, want %q", got, "tables/orders")
	}
	if !IsReserved("index.md") || !IsReserved("log.md") || IsReserved("orders.md") {
		t.Error("IsReserved classification wrong")
	}
}

// writeBundle materializes rel→content files under a temp dir and returns it.
func writeBundle(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// findings collects (path, severity, message-substring) presence checks.
func hasFinding(r *Report, path string, sev Severity, substr string) bool {
	for _, f := range r.Findings {
		if f.Path == path && f.Severity == sev && strings.Contains(f.Message, substr) {
			return true
		}
	}
	return false
}

func TestValidateBundle_Conformant(t *testing.T) {
	root := writeBundle(t, map[string]string{
		"index.md":            "---\nokf_version: 0.1\n---\n# Sales\n* [Orders](/tables/orders.md) - orders\n",
		"tables/index.md":     "# Tables\n* [Orders](orders.md) - one row per order\n",
		"tables/orders.md":    "---\ntype: BigQuery Table\ntitle: Orders\ntags: [sales, revenue]\n---\n# Orders\nJoined with [customers](/tables/customers.md).\n",
		"tables/customers.md": "---\ntype: BigQuery Table\ntitle: Customers\n---\n# Customers\n",
		"log.md":              "# Log\n## 2026-05-28\n- created orders\n",
	})
	report, err := ValidateBundle(root)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Conforms() {
		t.Errorf("expected conformant bundle, findings: %+v", report.Findings)
	}
	if report.Files != 5 {
		t.Errorf("Files = %d, want 5", report.Files)
	}
}

func TestValidateBundle_Errors(t *testing.T) {
	root := writeBundle(t, map[string]string{
		"no_fm.md":     "# No frontmatter\n",
		"no_type.md":   "---\ntitle: Untyped\n---\n# Body\n",
		"sub/index.md": "---\ntype: Nope\n---\n# Sub\n",
		"good.md":      "---\ntype: Doc\n---\n# Good\nlink to [missing](/gone.md)\n",
	})
	report, err := ValidateBundle(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Conforms() {
		t.Fatal("expected non-conformant bundle")
	}
	checks := []struct {
		path string
		sev  Severity
		sub  string
	}{
		{"no_fm.md", Error, "missing frontmatter"},
		{"no_type.md", Error, "missing required `type`"},
		{"sub/index.md", Error, "must not contain frontmatter"},
		{"good.md", Warn, "broken link: /gone.md"},
	}
	for _, c := range checks {
		if !hasFinding(report, c.path, c.sev, c.sub) {
			t.Errorf("missing finding %s/%s containing %q; got %+v", c.path, c.sev, c.sub, report.Findings)
		}
	}
}

func TestValidateBundle_LogAndRootIndexWarnings(t *testing.T) {
	root := writeBundle(t, map[string]string{
		"index.md":   "---\nokf_version: 0.1\nbogus: x\n---\n# Root\n",
		"log.md":     "# Log\n## 05/28/2026\n- bad date\n",
		"bad_ts.md":  "---\ntype: Doc\ntimestamp: May 28 2026\n---\n# Body\n",
		"good_ts.md": "---\ntype: Doc\ntimestamp: 2026-05-28T14:30:00Z\n---\n# Body\n",
	})
	report, err := ValidateBundle(root)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Conforms() {
		t.Errorf("warnings should not make a bundle non-conformant: %+v", report.Findings)
	}
	if !hasFinding(report, "index.md", Warn, "bogus") {
		t.Errorf("expected root-index warning for non-OKF key; got %+v", report.Findings)
	}
	if !hasFinding(report, "log.md", Warn, "not ISO 8601") {
		t.Errorf("expected log.md non-ISO date warning; got %+v", report.Findings)
	}
	if !hasFinding(report, "bad_ts.md", Warn, "not RFC 3339") {
		t.Errorf("expected timestamp-format warning; got %+v", report.Findings)
	}
	if hasFinding(report, "good_ts.md", Warn, "RFC 3339") {
		t.Errorf("well-formed timestamp should not warn; got %+v", report.Findings)
	}
}

func TestValidateBundle_NotADirectory(t *testing.T) {
	if _, err := ValidateBundle(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("expected error for missing bundle path")
	}
}
