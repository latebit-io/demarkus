// Package driftreport audits each knowledge world served in process on a
// schedule and writes what drifted there, by owner, to one generated
// document in that world. It calls no model: drift is version arithmetic.
package driftreport

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/memory/audit"
	"github.com/latebit-io/demarkus/client/memory/worldpolicy"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

const (
	// retention keeps the recent reports: the trend, not an archive.
	retention = 30
	// readsPerSecond paces a pass well under the limiter the worlds share
	// with every in-process caller, the federation deriver among them.
	readsPerSecond = 5
	auditTimeout   = time.Hour

	summary = "Documents in this world whose pinned sources moved, by owner; generated on a schedule, so edits are overwritten."
)

// Grant is the write access a report is published under: its own path.
var Grant = protocol.Grant{Label: "drift-report", Paths: []string{protocol.DriftReportPath}}

// Store reads the worlds by name with no token, which in process passes a
// world's gate, and writes each report.
type Store interface {
	docwrite.Backend
	List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error)
}

// Config is the worlds to report on and how often.
type Config struct {
	Worlds []string
	Store  Store
	// Floor names the world whose write policy every other is merged under;
	// "" is none.
	Floor    string
	Interval time.Duration
	Log      *slog.Logger
}

// Run reports at start, so a new leader does not wait an interval, then
// Interval after each pass ends, until ctx ends. Callers run it under
// leader.Run so one replica writes.
func Run(ctx context.Context, cfg Config) { //nolint:gocritic // a config is passed once per term
	r := newReporter(cfg)
	for {
		r.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(cfg.Interval):
		}
	}
}

// reporter is a Config whose reads are paced.
type reporter struct {
	Config
}

func newReporter(cfg Config) *reporter { //nolint:gocritic // a config is passed once per term
	cfg.Log = cmp.Or(cfg.Log, slog.Default())
	cfg.Store = paced{Store: cfg.Store, limit: rate.NewLimiter(readsPerSecond, readsPerSecond)}
	return &reporter{Config: cfg}
}

// pass reports every world; one world's failure does not skip the others.
func (r *reporter) pass(ctx context.Context) {
	for _, world := range r.Worlds {
		if ctx.Err() != nil {
			return
		}
		outcome, err := r.report(ctx, world)
		if err != nil {
			r.Log.ErrorContext(ctx, "drift report failed", "world", world, "err", err)
			continue
		}
		r.Log.InfoContext(ctx, "drift report", "world", world, "outcome", outcome)
	}
}

// report audits one world and publishes its report when it differs from the
// one there; the outcome says which. An archived report is the world opting
// out, so nothing is written over it.
func (r *reporter) report(ctx context.Context, world string) (string, error) {
	floor, err := r.floorPolicy(ctx, world)
	if err != nil {
		return "", err
	}
	found, err := audit.Drift(ctx, &audit.ClientStore{Client: r.Store, Host: world}, audit.Options{Scope: "/", Floor: floor, Timeout: auditTimeout})
	if err != nil {
		return "", fmt.Errorf("audit: %w", err)
	}
	meta := generation.ArtifactMetadata("demarkus-drift", retention)
	meta["tags"], meta["importance"], meta["description"] = "drift,freshness,report", "0.3", summary
	body := Render(found)
	doc := docwrite.Doc{Backend: r.Store, Host: world, Path: protocol.DriftReportPath, Write: func(ctx context.Context, op docwrite.WriteOp) (fetch.Result, error) {
		return op(protocol.WithGrant(ctx, Grant), "")
	}}
	result, written, err := doc.Edit(ctx, nil, func(head *docwrite.Head) (docwrite.Write, bool, error) {
		return docwrite.Write{Body: body, Metadata: meta}, head.Body != body, nil
	})
	switch status := result.Response.Status; {
	case err != nil:
		return "", fmt.Errorf("publish %s: %w", protocol.DriftReportPath, err)
	case written:
		return fmt.Sprintf("published, %d documents drifted", found.Freshness.DocumentsDrifted), nil
	case status == protocol.StatusArchived:
		return "archived, left alone", nil
	case status == protocol.StatusOK:
		return "unchanged", nil
	}
	return "", fmt.Errorf("publish %s: %s", protocol.DriftReportPath, result.Response.Refusal())
}

// floorPolicy reads the floor world's policy, as the tools do, so the report
// counts the documents an on-demand audit counts; nil for the floor itself.
func (r *reporter) floorPolicy(ctx context.Context, world string) (*publishpolicy.Policy, error) {
	if r.Floor == "" || r.Floor == world {
		return nil, nil
	}
	policy, err := worldpolicy.Read(ctx, func(ctx context.Context, p string) (protocol.Response, error) {
		res, err := r.Store.Fetch(ctx, fetch.FetchRequest{Host: r.Floor, Path: p})
		return res.Response, err
	})
	if err != nil {
		return nil, fmt.Errorf("floor policy of %s: %w", r.Floor, err)
	}
	return policy, nil
}

// Render is the report document: the freshness baseline, then each drifted
// document under its owner. It carries no date, so an unchanged world
// renders the same body and writes no version.
func Render(r *audit.Report) string {
	var b strings.Builder
	b.WriteString("# Drift report\n\n" + summary + "\n\n## Freshness\n\n")
	for _, line := range r.Freshness.Lines() {
		b.WriteString("- " + line + "\n")
	}
	byOwner := map[string][]audit.Finding{}
	for _, f := range r.Findings {
		if f.Check == audit.CheckDrift {
			byOwner[f.Owner] = append(byOwner[f.Owner], f)
		}
	}
	owners := slices.Sorted(maps.Keys(byOwner))
	if len(owners) == 0 {
		b.WriteString("\nNothing has drifted.\n")
	} else if owners[0] == "" {
		owners = append(owners[1:], "") // the documents nobody owns last
	}
	for _, o := range owners {
		heading := "Owner: " + o
		if o == "" {
			heading = "No owner"
		}
		b.WriteString("\n## " + heading + "\n\n")
		for _, f := range byOwner[o] {
			fmt.Fprintf(&b, "- `%s`: %s\n", f.Path, f.Detail)
		}
	}
	if len(r.Coverage) > 0 {
		b.WriteString("\n## Coverage\n\nThe audit was partial:\n\n")
		for _, note := range r.Coverage {
			b.WriteString("- " + note + "\n")
		}
	}
	return b.String()
}

// paced waits for the limiter before every read of a pass.
type paced struct {
	Store
	limit *rate.Limiter
}

func (p paced) Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error) {
	if err := p.limit.Wait(ctx); err != nil {
		return fetch.Result{}, err
	}
	return p.Store.Fetch(ctx, r)
}

func (p paced) List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error) {
	if err := p.limit.Wait(ctx); err != nil {
		return fetch.Result{}, err
	}
	return p.Store.List(ctx, r)
}
