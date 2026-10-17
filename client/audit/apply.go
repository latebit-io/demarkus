package audit

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/hub"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
)

// Results an apply reports per planned write.
const (
	Applied    = "applied"
	Conflicted = "conflicted" // the document moved since the audit read it
	Refused    = "refused"    // the store or the transport said no
	Skipped    = "skipped"    // a write it requires did not land
)

// Outcome is one planned write after an apply, in the plan's order.
type Outcome struct {
	Path     string
	Result   string
	Version  int               // the version written, when applied
	Detail   string            // why, when not applied
	Metadata map[string]string // as sent plus the response's keys
}

// Apply writes the plan's batch under the surface's identity, each write at
// the version the audit read and failing on conflict. Writes are
// independent, except that one whose Requires did not land is skipped.
func Apply(ctx context.Context, docs hub.Docs, p *Plan, stamp map[string]string) []Outcome {
	landed := map[string]bool{}
	out := make([]Outcome, 0, len(p.Writes))
	for i := range p.Writes {
		w := &p.Writes[i]
		if missing := slices.DeleteFunc(slices.Clone(w.Requires), func(r string) bool { return landed[r] }); len(missing) > 0 {
			out = append(out, Outcome{Path: w.Path, Result: Skipped, Detail: "needs " + strings.Join(missing, ", ") + ", which did not land"})
			continue
		}
		o := publishPlanned(ctx, docs(w.Path), w, stamp)
		landed[w.Path] = o.Result == Applied
		out = append(out, o)
	}
	return out
}

func publishPlanned(ctx context.Context, doc *docwrite.Doc, w *PlanWrite, stamp map[string]string) Outcome {
	meta := maps.Clone(w.Metadata)
	maps.Copy(meta, stamp)
	o := Outcome{Path: w.Path, Metadata: meta}
	result, err := doc.Publish(ctx, docwrite.Write{Body: w.Body, ExpectedVersion: w.Version, Metadata: meta}, merge.OnConflictFail)
	status := result.Response.Status
	switch {
	case err != nil:
		o.Result, o.Detail = Refused, err.Error()
	case status == protocol.StatusConflict && w.Version == 0:
		o.Result, o.Detail = Conflicted, "exists now; rerun the proposal"
	case status == protocol.StatusConflict:
		o.Result, o.Detail = Conflicted, fmt.Sprintf("changed since v%d; rerun the proposal", w.Version)
	case !protocol.IsWriteSuccess(status):
		o.Result, o.Detail = Refused, status
	default:
		o.Result = Applied
		maps.Copy(o.Metadata, result.Response.Metadata)
		if v, err := docwrite.Version(result.Response.Metadata); err != nil {
			o.Detail = "landed, version unknown: " + err.Error()
		} else {
			o.Version = v
		}
	}
	return o
}
