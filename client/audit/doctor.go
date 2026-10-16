// Package doctor audits a demarkus store for catalog hygiene: broken links,
// orphans, missing hubs, untagged and untyped documents, hub and document
// shape, ADR gaps and references. It reads every document in scope once and
// derives every check from that pass, so the report is exact within its
// bounds. It never writes.
package audit

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/layout"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
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
	// MaxShownPerCheck caps one section of the Markdown report; the JSON
	// report is complete. A whole-store run otherwise outgrows an MCP result.
	MaxShownPerCheck = 20
	MaxAuditDuration = 10 * time.Minute
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
	Scope     string        // "/" or a directory path of slugs, "/<slug>/"
	Deep      bool          // metadata lost across versions
	Promotion bool          // documents worth lifting to a shared knowledge system
	Timeout   time.Duration // whole audit; 0 is MaxAuditDuration
	// Floor is the policy the store's own policy is merged over (the floor
	// world's, behind a knowledge broker); nil means none.
	Floor *publishpolicy.Policy
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
	// Promoted counts the documents the promotion check excluded as already
	// promoted; it reads on the summary line only when that check ran.
	Promotion bool `json:"promotion"`
	Promoted  int  `json:"promoted,omitempty"`
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
	promoted int // documents the promotion check excluded as already promoted
	layout   layout.Layout
	// policy is the effective write policy (the floor merged under the
	// store's own); policyKnown is false when the store's could not be read.
	policy      publishpolicy.Policy
	policyKnown bool
}

// IsSlug reports whether s can be a store path segment; the layout owns the rule.
func IsSlug(s string) bool { return layout.IsSegment(s) }

// ValidScope checks the audit root: "/" or a directory path whose every
// segment is a path-safe slug, returned with its trailing slash.
func ValidScope(scope string) (string, error) {
	if scope == "" || scope == "/" {
		return "/", nil
	}
	clean := strings.Trim(scope, "/")
	for seg := range strings.SplitSeq(clean, "/") {
		if !IsSlug(seg) {
			return "", fmt.Errorf("scope %q is not a slug path", scope)
		}
	}
	return "/" + clean + "/", nil
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
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = MaxAuditDuration
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	a := &audit{opts: opts, store: store, docs: map[string]*document{}, dirs: map[string]bool{}}
	if a.layout, err = layout.Read(ctx, store.Fetch); err != nil {
		return nil, err
	}
	a.readPolicy(ctx)
	if err := a.inventory(ctx); err != nil {
		return nil, err
	}
	if err := a.fetchAll(ctx); err != nil {
		return nil, err
	}
	a.checkLinks(ctx)
	a.checkOrphans()
	a.checkHubs(ctx)
	a.checkTitles()
	a.checkADRSequence()
	a.checkMetadata()
	a.checkPolicy()
	a.checkDuplicates()
	a.checkShape()
	a.checkContent()
	a.checkStyleGuide(ctx)
	a.checkReferences()
	if opts.Deep {
		a.checkLostMetadata(ctx)
	}
	if opts.Promotion {
		a.checkPromotion()
	}
	return &Report{Scope: scope, Documents: len(a.order), Deep: opts.Deep, Promotion: opts.Promotion, Promoted: a.promoted, Findings: a.findings, Coverage: a.coverage}, nil
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
		if !live(resp.Status) {
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
		target := links.ResolvePath(d.path, dest)
		if target == "" || seen[target] {
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

func live(status string) bool { return status == protocol.StatusOK }
