// Package journal is the dated entry mechanic every memory surface shares:
// today's document under a scope, created or appended once, its tags
// widened, and the hubs that reach it kept current. Surfaces word the outcome.
package journal

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/hub"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/layout"
)

// Request is one entry for today's journal under Dir ("/" or "/<slug>/").
type Request struct {
	Dir   string
	Entry string
	Tags  []string          // subjects of the entry; journal and the slug are implied
	Stamp map[string]string // the surface's identity keys, on every write
	Title string            // the scope's name when it has no hub; "" is Memory
	Now   time.Time         // zero is time.Now; the date is UTC
	// Layout names the journal directory; zero is the default layout.
	Layout layout.Layout
}

// Status is how the entry ended.
const (
	StatusCreated  = "created"  // today's document was created for it
	StatusAppended = "appended" // added to today's document
	StatusPresent  = "present"  // already there as its own block; nothing written
)

// Outcome is where the entry landed. Notes are hub links that failed after
// the entry was committed: reported, never retried by resending the entry.
type Outcome struct {
	Status  string
	Path    string
	Version int
	Notes   []string
}

// Docs binds the surface's write contract to one path on the store.
type Docs func(docPath string) *docwrite.Doc

const (
	defaultImportance = "0.4"
	hubImportance     = "0.8"
	rootImportance    = "0.9"
	rootTitle         = "Memory"
)

// Append records the entry. An error means nothing was committed, or the
// outcome is unknown; a committed entry always returns an Outcome.
func Append(ctx context.Context, docs Docs, req Request) (Outcome, error) { //nolint:gocritic // a request is built once and passed once
	req.Entry = strings.TrimSpace(req.Entry)
	if req.Entry == "" {
		return Outcome{}, errors.New("entry is empty")
	}
	if !strings.HasSuffix(req.Dir, "/") {
		req.Dir += "/"
	}
	if req.Now.IsZero() {
		req.Now = time.Now()
	}
	if req.Layout.Journal == "" {
		req.Layout = layout.Default()
	}
	r := run{Request: req, docs: docs, date: req.Now.UTC().Format("2006-01-02")}

	scopeHub := docs(r.Dir + "index.md")
	hubHead, err := head(ctx, scopeHub)
	if err != nil {
		return Outcome{}, err
	}
	r.title = r.titleFrom(hubHead)

	doc := docs(r.Dir + r.Layout.Journal + r.date + ".md")
	out := Outcome{Path: doc.Path}
	if out.Status, out.Version, err = r.record(ctx, doc); err != nil {
		return Outcome{}, err
	}
	for _, link := range r.links() {
		h := hubHead
		if link.doc.Path != scopeHub.Path {
			h = nil
		}
		if err := hub.EnsureLink(ctx, link.doc, h, &link.Link); err != nil {
			out.Notes = append(out.Notes, err.Error())
		}
	}
	return out, nil
}

// run is one request with its day and the scope's name resolved.
type run struct {
	Request
	docs  Docs
	date  string
	title string
}

// record creates today's document or appends to it; a creation conflict
// means another writer got there first, so the entry is appended instead.
func (r *run) record(ctx context.Context, doc *docwrite.Doc) (status string, version int, err error) {
	h, err := head(ctx, doc)
	if err != nil {
		return "", 0, err
	}
	if h.Status == protocol.StatusNotFound {
		created, version, err := r.create(ctx, doc)
		if err != nil || created {
			return StatusCreated, version, err
		}
		if h, err = head(ctx, doc); err != nil {
			return "", 0, err
		}
	}
	if h.Status != protocol.StatusOK {
		return "", 0, fmt.Errorf("fetch %s: %s", doc.Path, h.Status)
	}
	return r.appendOnce(ctx, doc, h)
}

func (r *run) create(ctx context.Context, doc *docwrite.Doc) (created bool, version int, err error) {
	w := docwrite.Write{
		Body: fmt.Sprintf("# %s journal: %s\n\n%s\n", r.title, r.date, r.Entry),
		Metadata: r.stamped(map[string]string{
			"tags":       strings.Join(mergeTags([]string{"journal", slugOf(r.Dir)}, r.Tags), ","),
			"importance": defaultImportance,
			"type":       "Journal",
		}),
	}
	result, err := doc.Publish(ctx, w, merge.OnConflictFail)
	switch {
	case err != nil:
		return false, 0, fmt.Errorf("create %s: %w", doc.Path, err)
	case result.Response.Status == protocol.StatusConflict:
		return false, 0, nil
	case protocol.IsWriteSuccess(result.Response.Status):
		version, err = docwrite.Version(result.Response.Metadata)
		return true, version, err
	}
	return false, 0, fmt.Errorf("create %s: %s", doc.Path, result.Response.Status)
}

// appendOnce adds the entry unless it is already there as its own block; the
// append carries the widened tags, which the server merges over the base.
// One retry after a conflict, against the refetched head.
func (r *run) appendOnce(ctx context.Context, doc *docwrite.Doc, h *docwrite.Head) (status string, version int, err error) {
	for attempt := range 2 {
		if hasBlock(h.Body, r.Entry) {
			return StatusPresent, h.Version, nil
		}
		meta := r.stamped(map[string]string{"tags": strings.Join(mergeTags(protocol.SplitTags(h.Metadata["tags"]), r.Tags), ",")})
		result, err := doc.Append(ctx, docwrite.AppendRequest{Body: "\n" + r.Entry + "\n", ExpectedVersion: h.Version, Metadata: meta})
		switch {
		case err != nil:
			return "", 0, fmt.Errorf("append %s: %w", doc.Path, err)
		case protocol.IsWriteSuccess(result.Response.Status):
			version, err = docwrite.Version(result.Response.Metadata)
			return StatusAppended, version, err
		case result.Response.Status != protocol.StatusConflict:
			return "", 0, fmt.Errorf("append %s: %s", doc.Path, result.Response.Status)
		case attempt == 1:
			return "", 0, fmt.Errorf("append %s: conflict twice; not written", doc.Path)
		}
		if h, err = head(ctx, doc); err != nil {
			return "", 0, err
		}
		if h.Status != protocol.StatusOK {
			return "", 0, fmt.Errorf("fetch %s after a conflict: %s", doc.Path, h.Status)
		}
	}
	return "", 0, errors.New("unreachable")
}

// hubLink is one hub to keep current.
type hubLink struct {
	doc *docwrite.Doc
	hub.Link
}

// links are the hubs the entry must be reachable from: the scope hub links
// the journal directory (one link reaches every day), the root hub links the
// project.
func (r *run) links() []hubLink {
	section := layout.Section(r.Layout.Journal)
	bullet := fmt.Sprintf("- [%s](%s): dated session notes", section, r.Layout.Journal)
	out := []hubLink{{doc: r.docs(r.Dir + "index.md"), Link: hub.Link{
		Target: r.Dir + r.Layout.Journal, Bullet: bullet, Section: "## " + section,
		CreateBody: fmt.Sprintf("# %s\n\n## %s\n\n%s\n", r.title, section, bullet),
		CreateMeta: map[string]string{"tags": strings.Join(mergeTags([]string{"index"}, []string{slugOf(r.Dir)}), ","), "importance": hubImportance},
		Stamp:      r.Stamp,
	}}}
	if r.Dir == "/" {
		return out
	}
	return append(out, hubLink{doc: r.docs("/index.md"), Link: hub.Link{
		Target: r.Dir, Bullet: fmt.Sprintf("- [%s](%s)", r.title, r.Dir),
		CreateBody: fmt.Sprintf("# Projects\n\n- [%s](%s)\n", r.title, r.Dir),
		CreateMeta: map[string]string{"tags": "projects,index", "importance": rootImportance},
		Stamp:      r.Stamp,
	}})
}

// titleFrom names the scope: the hub's name, else the slug, else the
// request's title, else Memory.
func (r *run) titleFrom(h *docwrite.Head) string {
	fallback := slugOf(r.Dir)
	if fallback == "" {
		fallback = r.Title
	}
	if fallback == "" {
		fallback = rootTitle
	}
	if h.Status != protocol.StatusOK {
		return fallback
	}
	return hub.Title(h.Body, h.Metadata, fallback)
}

func (r *run) stamped(meta map[string]string) map[string]string {
	maps.Copy(meta, r.Stamp)
	return meta
}

// head reads the document as an edit would find it.
func head(ctx context.Context, doc *docwrite.Doc) (*docwrite.Head, error) {
	res, err := doc.Backend.Fetch(ctx, fetch.FetchRequest{Host: doc.Host, Path: doc.Path, Token: doc.ReadToken})
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", doc.Path, err)
	}
	version, err := docwrite.Version(res.Response.Metadata)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", doc.Path, err)
	}
	return &docwrite.Head{Status: res.Response.Status, Body: res.Response.Body, Version: version, Metadata: res.Response.Metadata}, nil
}

// hasBlock reports the entry present as its own blank line delimited block.
func hasBlock(body, entry string) bool {
	if !strings.Contains(body, entry) {
		return false
	}
	for block := range strings.SplitSeq(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n") {
		if strings.TrimSpace(block) == entry {
			return true
		}
	}
	return false
}

// slugOf is the last segment of a project directory; "" at the root.
func slugOf(dir string) string {
	if dir == "/" {
		return ""
	}
	return path.Base(strings.TrimSuffix(dir, "/"))
}

// mergeTags adds the trimmed, non-empty tags of add that have is missing.
func mergeTags(have, add []string) []string {
	out := slices.DeleteFunc(slices.Clone(have), func(t string) bool { return t == "" })
	for _, t := range add {
		if t = strings.TrimSpace(t); t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}
