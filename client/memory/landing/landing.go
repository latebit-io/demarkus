// Package landing is what a write does beyond the store's verb: a publish is
// held to the style rules, linked from its hub and checked for narrowed
// metadata; an archive unlinks what it retired. Every surface that writes
// runs these, so a document lands the same from a tool, the CLI or a script.
package landing

import (
	"context"
	"fmt"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/memory/gate"
	"github.com/latebit-io/demarkus/client/memory/hub"
	"github.com/latebit-io/demarkus/client/memory/metaguard"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

// Style is how a publish is held to the style rules: Block refuses the
// write, anything else lets it land with the findings. Guide is what a
// finding cites; "" cites gate.Baseline.
type Style struct {
	Strictness publishpolicy.Strictness
	Guide      string
}

// StyleRefusal is a publish the style gate blocked; nothing was sent.
type StyleRefusal struct{ Findings string }

func (e *StyleRefusal) Error() string { return "style problems: " + e.Findings }

// PublishRequest is one publish and what lands with it.
type PublishRequest struct {
	Path  string
	Write docwrite.Write
	Mode  merge.OnConflict
	// Style is asked only when the body has style problems: a surface may
	// read a policy document to answer. Nil warns and cites the baseline.
	Style func() Style
	// Stamp is the identity the hub edit is written under.
	Stamp map[string]string
}

// Published is a publish the server answered. Landed is false for a merge
// candidate or a refusing status: nothing else was done then.
type Published struct {
	Result docwrite.Result
	Landed bool
	// StyleFindings are the style problems a landed write carries; "" for none.
	StyleFindings string
	Hub           HubLink
	// Narrowed names the tags or keys the write dropped from the version it
	// replaced; NarrowedErr is that check failing, which never fails the write.
	Narrowed    string
	NarrowedErr error
}

// HubLink is what the hub step of a first version did: a bullet added to
// Hub, or Problem saying why no hub names the document. Both empty when a
// hub named it already or the step did not apply.
type HubLink struct {
	Hub     string
	Added   bool
	Problem string
}

// Beside binds doc's write contract to other paths on its store: a hub
// shares its document's tokens, one authorization, one store.
func Beside(doc *docwrite.Doc) hub.Docs {
	return func(p string) *docwrite.Doc {
		other := *doc
		other.Path = p
		return &other
	}
}

// Landed is a write the server applied, not a merge candidate to review.
func Landed(r *docwrite.Result) bool {
	return r.Candidate == nil && protocol.IsWriteSuccess(r.Response.Status)
}

// Publish runs the style gate, sends the write, and for a landed one links
// a first version from its hub and checks what the update dropped. The
// error is a *StyleRefusal or the write's own failure.
func Publish(ctx context.Context, docs hub.Docs, req *PublishRequest) (Published, error) {
	var out Published
	if problems := gate.Problems(req.Path, req.Write.Body, req.Write.Metadata["description"]); len(problems) > 0 {
		style := Style{Strictness: publishpolicy.Warn}
		if req.Style != nil {
			style = req.Style()
		}
		if style.Guide == "" {
			style.Guide = gate.Baseline
		}
		out.StyleFindings = gate.Findings(problems, style.Guide)
		if style.Strictness == publishpolicy.Block {
			return Published{}, &StyleRefusal{Findings: out.StyleFindings}
		}
	}
	doc := docs(req.Path)
	result, err := doc.Publish(ctx, req.Write, req.Mode)
	if err != nil {
		return Published{}, err
	}
	out.Result, out.Landed = result, Landed(&result)
	if !out.Landed {
		out.StyleFindings = ""
		return out, nil
	}
	out.Hub = linkFirstVersion(ctx, docs, req, &result)
	out.Narrowed, out.NarrowedErr = metaguard.Gate(ctx, req.Write.ExpectedVersion, req.Write.Metadata, func(ctx context.Context) (fetch.Result, error) {
		return doc.FetchVersion(ctx, req.Write.ExpectedVersion)
	})
	return out, nil
}

// linkFirstVersion links a first version from the nearest ancestor hub that
// exists, unless it names the document already (its directory counts). A hub
// at the cap, not editable, or absent is a Problem; the publish stands.
func linkFirstVersion(ctx context.Context, docs hub.Docs, req *PublishRequest, result *docwrite.Result) HubLink {
	version, err := docwrite.Version(result.Response.Metadata)
	if err != nil || version != 1 || !hub.Linkable(req.Path) {
		return HubLink{}
	}
	for _, hubPath := range hub.Ancestors(req.Path) {
		doc := docs(hubPath)
		h, err := doc.Head(ctx)
		if err != nil {
			return HubLink{Hub: hubPath, Problem: fmt.Sprintf("hub %s not updated: %v", hubPath, err)}
		}
		switch h.Status {
		case protocol.StatusNotFound, protocol.StatusArchived:
			continue // the next ancestor may name it
		case protocol.StatusOK:
		default:
			return HubLink{Hub: hubPath, Problem: fmt.Sprintf("hub %s not updated: %s", hubPath, h.Status)}
		}
		added, err := hub.EnsureLink(ctx, doc, h, &hub.Link{Target: req.Path, Bullet: hub.Bullet(req.Path, req.Write.Body, req.Write.Metadata), Stamp: req.Stamp})
		if err != nil {
			return HubLink{Hub: hubPath, Problem: err.Error()}
		}
		return HubLink{Hub: hubPath, Added: added}
	}
	return HubLink{Problem: unlinked(req.Path)}
}

// unlinked words the finding when no ancestor hub exists, with the fix.
func unlinked(p string) string {
	nearest := hub.Ancestors(p)[0]
	fix := "publish " + nearest + " with a bullet for it"
	if nearest != "/index.md" {
		fix += ", or add one to /index.md"
	}
	return "unlinked: no hub reaches " + p + "; " + fix
}
