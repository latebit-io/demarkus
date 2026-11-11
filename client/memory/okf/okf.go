// Package okf reads and validates Open Knowledge Format (OKF) bundles — a
// directory tree of markdown files with YAML frontmatter, cross-linked into a
// relationship graph — for the demarkus okf validate, import and export
// commands, with a conformance validator for OKF v0.1.
//
// Spec: https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md
package okf

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/memory/frontmatter"
)

// Reserved bundle filenames with defined structure. Every other ".md" file is a
// concept document.
const (
	IndexFile = "index.md" // directory listing for progressive disclosure
	LogFile   = "log.md"   // chronological change history
)

// IsReserved reports whether a file base name is an OKF reserved filename.
func IsReserved(base string) bool {
	return base == IndexFile || base == LogFile
}

// ConceptID returns the OKF concept identity for a bundle-relative markdown
// path: the path with its ".md" suffix removed (e.g. "tables/orders.md" →
// "tables/orders").
func ConceptID(relPath string) string {
	return strings.TrimSuffix(filepath.ToSlash(relPath), ".md")
}

// SplitFrontmatter is frontmatter.Split, kept here for bundle readers.
func SplitFrontmatter(data []byte) (fm string, body []byte, ok bool) {
	return frontmatter.Split(data)
}

// ParseFrontmatter is frontmatter.Parse, kept here for bundle readers.
func ParseFrontmatter(fm string) (map[string]string, error) { return frontmatter.Parse(fm) }

// Severity classifies a validation finding. Error means the bundle does not
// conform to OKF v0.1; Warn is soft guidance the spec asks consumers to tolerate.
type Severity int

// Severity levels: Warn is soft guidance the spec asks consumers to tolerate;
// Error means the bundle does not conform to OKF v0.1.
const (
	Warn Severity = iota
	Error
)

func (s Severity) String() string {
	if s == Error {
		return "error"
	}
	return "warning"
}

// Finding is a single validation result against a bundle file.
type Finding struct {
	Path     string // bundle-relative file path
	Severity Severity
	Message  string
}

// Report is the result of validating a bundle.
type Report struct {
	Files    int
	Findings []Finding
}

// Counts tallies findings by severity.
func (r *Report) Counts() (errors, warnings int) {
	for _, f := range r.Findings {
		if f.Severity == Error {
			errors++
		} else {
			warnings++
		}
	}
	return errors, warnings
}

// Conforms reports whether the bundle has no Error-level findings.
func (r *Report) Conforms() bool {
	errors, _ := r.Counts()
	return errors == 0
}

// ValidateBundle walks an OKF bundle directory and checks it against the v0.1
// conformance rules: every non-reserved ".md" has parseable frontmatter with a
// non-empty `type`, and reserved filenames follow their defined structure.
// Broken cross-links are reported as warnings, per the spec's requirement that
// consumers tolerate them.
func ValidateBundle(root string) (*Report, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat bundle: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("bundle path is not a directory: %s", root)
	}

	type mdFile struct {
		rel  string
		data []byte
	}
	var mdFiles []mdFile
	files := map[string]bool{}
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mdFiles = append(mdFiles, mdFile{rel, data})
		files[rel] = true
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk bundle: %w", walkErr)
	}

	report := &Report{Files: len(mdFiles)}
	for _, f := range mdFiles {
		body := f.data
		switch path.Base(f.rel) {
		case IndexFile:
			report.Findings = append(report.Findings, validateIndex(f.rel, f.data)...)
			_, body, _ = SplitFrontmatter(f.data)
		case LogFile:
			report.Findings = append(report.Findings, validateLog(f.rel, f.data)...)
		default:
			report.Findings = append(report.Findings, validateConcept(f.rel, f.data)...)
			_, body, _ = SplitFrontmatter(f.data)
		}
		report.Findings = append(report.Findings, checkLinks(f.rel, body, files)...)
	}

	sort.Slice(report.Findings, func(i, j int) bool {
		a, b := report.Findings[i], report.Findings[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Message < b.Message
	})
	return report, nil
}

func validateConcept(rel string, data []byte) []Finding {
	fm, _, ok := SplitFrontmatter(data)
	if !ok {
		return []Finding{{rel, Error, "missing frontmatter; OKF concept documents require a frontmatter block with a non-empty `type`"}}
	}
	meta, err := ParseFrontmatter(fm)
	if err != nil {
		return []Finding{{rel, Error, fmt.Sprintf("unparseable frontmatter: %v", err)}}
	}
	var out []Finding
	if strings.TrimSpace(meta["type"]) == "" {
		out = append(out, Finding{rel, Error, "missing required `type` field"})
	}
	if ts := meta["timestamp"]; ts != "" && !isRFC3339(ts) {
		out = append(out, Finding{rel, Warn, fmt.Sprintf("timestamp %q is not RFC 3339 (ISO 8601)", ts)})
	}
	return out
}

func isRFC3339(s string) bool {
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

func validateIndex(rel string, data []byte) []Finding {
	fm, _, ok := SplitFrontmatter(data)
	if !ok {
		return nil
	}
	// Only the root index.md may carry frontmatter, and only to declare
	// okf_version.
	if rel != IndexFile {
		return []Finding{{rel, Error, "index.md must not contain frontmatter (reserved navigation file)"}}
	}
	meta, err := ParseFrontmatter(fm)
	if err != nil {
		return []Finding{{rel, Error, fmt.Sprintf("unparseable frontmatter: %v", err)}}
	}
	var out []Finding
	for k := range meta {
		if k != "okf_version" {
			out = append(out, Finding{rel, Warn, fmt.Sprintf("root index.md frontmatter key %q is not defined by OKF (only okf_version)", k)})
		}
	}
	return out
}

func validateLog(rel string, data []byte) []Finding {
	var out []Finding
	for raw := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if !strings.HasPrefix(trimmed, "#") {
			continue
		}
		heading := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		// Only headings that look like a date (start with a digit) are checked.
		if heading == "" || heading[0] < '0' || heading[0] > '9' {
			continue
		}
		if first := strings.Fields(heading)[0]; !isISODate(first) {
			out = append(out, Finding{rel, Warn, fmt.Sprintf("log.md date heading %q is not ISO 8601 (YYYY-MM-DD)", first)})
		}
	}
	return out
}

func isISODate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func checkLinks(rel string, body []byte, files map[string]bool) []Finding {
	var out []Finding
	dir := path.Dir(rel)
	for _, dest := range links.Extract(string(body)) {
		if dest == "" || strings.Contains(dest, "://") || strings.HasPrefix(dest, "mailto:") {
			continue
		}
		// links.Extract already strips fragments; drop any query/fragment too so
		// a "/x.md?ref=1" link still resolves to the document path "/x.md".
		if i := strings.IndexAny(dest, "?#"); i >= 0 {
			dest = dest[:i]
		}
		if !strings.HasSuffix(dest, ".md") {
			continue
		}
		var target string
		if rest, ok := strings.CutPrefix(dest, "/"); ok {
			target = rest
		} else {
			target = path.Join(dir, dest)
		}
		target = path.Clean(target)
		if target == ".." || strings.HasPrefix(target, "../") {
			out = append(out, Finding{rel, Warn, fmt.Sprintf("link escapes bundle: %s", dest)})
			continue
		}
		if !files[target] {
			out = append(out, Finding{rel, Warn, fmt.Sprintf("broken link: %s", dest)})
		}
	}
	return out
}
