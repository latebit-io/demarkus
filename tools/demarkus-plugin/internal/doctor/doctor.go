// Package doctor audits a demarkus store for catalog hygiene: broken links,
// orphans, missing hubs, untagged and untyped documents, hub and document
// shape, ADR gaps and references. It reads every document in scope once and
// derives every check from that pass, so the report is exact within its
// bounds. It never writes.
package doctor

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"
)

// Bounds keep one audit affordable on a large store; each bound hit is
// reported under Coverage, never silently truncated.
const (
	MaxListCalls      = 1000
	MaxDocuments      = 10000
	MaxBodyBytes      = 64 << 20
	MaxLinkConfirms   = 50
	MaxVersionFetches = 100
	MaxVersionsPerDoc = 10
	MaxAuditDuration  = 10 * time.Minute
	// maxUnconfirmedShown caps the pairs listed in the coverage note; the count stays exact.
	maxUnconfirmedShown = 10
	oversizedBytes      = 8 * 1024
	adrNumberWidth      = 4
)

var (
	adrReferenceRe = regexp.MustCompile(`(?i)\bADR[ -]?#?(\d{3,4})\b`)
	adrFileRe      = regexp.MustCompile(`^(\d{4})-.*\.md$`)
)

// Store is the read surface an audit needs. Every method returns the wire
// response so callers see status text, not a translated error.
type Store interface {
	List(ctx context.Context, dir string, includeArchived bool, cursor string) (protocol.Response, error)
	Fetch(ctx context.Context, docPath string) (protocol.Response, error)
	Versions(ctx context.Context, docPath string) (protocol.Response, error)
}

// Options select the audit scope and optional checks.
type Options struct {
	Scope string // "/" or "/<slug>/"
	Deep  bool   // metadata lost across versions
}

// Finding is one report line: the document, what is wrong, and the fix.
type Finding struct {
	Check  string `json:"check"`
	Path   string `json:"path"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Report is the audit result. Coverage lists every bound hit and every call
// that failed, so an empty findings list is only "all clean" when Coverage
// is empty too.
type Report struct {
	Scope     string    `json:"scope"`
	Documents int       `json:"documents"`
	Findings  []Finding `json:"findings"`
	Coverage  []string  `json:"coverage"`
	Deep      bool      `json:"deep"`
}

// document is one inventory entry after its fetch.
type document struct {
	path     string
	status   string
	body     string
	meta     map[string]string
	headings []mdoutline.Heading
	links    []string // internal document targets, absolute, deduplicated and sorted
	dirLinks []string // internal directory targets, with trailing slash
}

type audit struct {
	opts     Options
	store    Store
	docs     map[string]*document
	order    []string
	dirs     map[string]bool
	findings []Finding
	coverage []string
}

// ValidScope checks the audit root: "/" or "/<slug>/" with a path-safe slug.
func ValidScope(scope string) (string, error) {
	if scope == "" || scope == "/" {
		return "/", nil
	}
	slug := strings.Trim(scope, "/")
	if strings.Contains(slug, "/") || !project.IsSlug(slug) {
		return "", fmt.Errorf("scope %q is not a project slug", scope)
	}
	return "/" + slug + "/", nil
}

// Run performs the audit. An error means the audit cannot be trusted
// (inventory failed, store unreachable, unauthorized); no partial report is
// returned with it.
func Run(ctx context.Context, store Store, opts Options) (*Report, error) {
	scope, err := ValidScope(opts.Scope)
	if err != nil {
		return nil, err
	}
	opts.Scope = scope
	ctx, cancel := context.WithTimeout(ctx, MaxAuditDuration)
	defer cancel()
	a := &audit{opts: opts, store: store, docs: map[string]*document{}, dirs: map[string]bool{}}
	if err := a.inventory(ctx); err != nil {
		return nil, err
	}
	if err := a.fetchAll(ctx); err != nil {
		return nil, err
	}
	a.checkLinks(ctx)
	a.checkOrphans()
	a.checkHubs()
	a.checkTitles()
	a.checkADRSequence()
	a.checkMetadata()
	a.checkDuplicates()
	a.checkShape()
	a.checkReferences()
	if opts.Deep {
		a.checkLostMetadata(ctx)
	}
	return &Report{Scope: scope, Documents: len(a.order), Deep: opts.Deep, Findings: a.findings, Coverage: a.coverage}, nil
}

func (a *audit) note(format string, args ...any) {
	a.coverage = append(a.coverage, fmt.Sprintf(format, args...))
}

func (a *audit) add(check, docPath, detail, fix string) {
	a.findings = append(a.findings, Finding{Check: check, Path: docPath, Detail: detail, Fix: fix})
}

// fetchAll reads every inventory document once, within the body budget.
func (a *audit) fetchAll(ctx context.Context) error {
	var bytes int64
	for i, p := range a.order {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("fetch pass stopped after %d of %d documents: %w", i, len(a.order), err)
		}
		if bytes >= MaxBodyBytes {
			a.note("body budget of %d MB reached after %d of %d documents; the rest were not read", MaxBodyBytes>>20, i, len(a.order))
			a.order = a.order[:i]
			break
		}
		d := a.docs[p]
		resp, err := a.store.Fetch(ctx, p)
		if err != nil {
			d.status = "error"
			a.note("fetch %s failed: %v", p, err)
			continue
		}
		d.status, d.meta = resp.Status, resp.Metadata
		if resp.Status == protocol.StatusUnauthorized {
			return fmt.Errorf("fetch %s returned unauthorized", p)
		}
		if !readableStatus(resp.Status) {
			a.note("fetch %s returned %s", p, resp.Status)
			continue
		}
		d.body = resp.Body
		bytes += int64(len(resp.Body))
		d.headings = mdoutline.Headings(resp.Body)
		a.classifyLinks(d)
	}
	return nil
}

// classifyLinks splits a body's links into documents and directories on this
// store. links.Extract already drops fragments; mark:// authorities are other
// stores and count as external.
func (a *audit) classifyLinks(d *document) {
	seen := map[string]bool{}
	for _, dest := range links.Extract(d.body) {
		if strings.Contains(dest, "://") {
			continue
		}
		target := path.Clean(dest)
		if !strings.HasPrefix(dest, "/") {
			target = path.Join(path.Dir(d.path), dest)
		}
		if seen[target] {
			continue
		}
		seen[target] = true
		if dir := strings.TrimSuffix(target, "/") + "/"; a.dirs[dir] {
			d.dirLinks = append(d.dirLinks, dir)
		} else {
			d.links = append(d.links, target)
		}
	}
	d.links = sortedUnique(d.links)
}

// title is the metadata title, else the first H1.
func title(d *document) string {
	if t := d.meta["title"]; t != "" {
		return t
	}
	for _, h := range d.headings {
		if h.Level == 1 {
			return h.Text
		}
	}
	return ""
}

func readableStatus(status string) bool {
	return status == protocol.StatusOK || status == protocol.StatusArchived
}

func (a *audit) hub() string {
	return path.Join(a.opts.Scope, "index.md")
}
