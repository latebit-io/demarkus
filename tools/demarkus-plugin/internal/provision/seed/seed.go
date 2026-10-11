// Package seed publishes the memory's first documents through the protocol.
package seed

import (
	"context"
	"embed"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/progress"
)

//go:embed index.md
var seedFS embed.FS

// Client is the protocol surface seeding needs; *fetch.Client satisfies it.
type Client interface {
	Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error)
	Publish(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error)
}

// seedSpec names one embedded seed document and where it goes.
type seedSpec struct {
	Host, Token string
	Name        string // embedded file, published at the memory root
	Meta        map[string]string
}

// MemoryDocs seeds index.md at host via PUBLISH so it gets version history; a
// flat file on disk is not FETCHable (issue #288). Best-effort: failures
// warn and provision continues.
func MemoryDocs(ctx context.Context, cl Client, host, token string) {
	seedDoc(ctx, cl, seedSpec{
		Host: host, Token: token, Name: "index.md",
		Meta: map[string]string{
			"tags":       "index,hub,projects,navigation",
			"importance": "0.9",
			// no OKF type: hubs stay untyped
		},
	})
}

// seedDoc publishes the embedded seed for name at the memory root unless the
// server already serves it. Create-only.
func seedDoc(ctx context.Context, cl Client, seed seedSpec) {
	name, docPath := seed.Name, "/"+seed.Name
	res, err := cl.Fetch(ctx, fetch.FetchRequest{Host: seed.Host, Path: docPath, Token: seed.Token})
	if err != nil {
		progress.Warnf("could not check %s before seeding (server unreachable?): %v", docPath, err)
		return
	}
	switch res.Response.Status {
	case protocol.StatusNotFound:
		// not served; seed below
	case protocol.StatusOK, protocol.StatusArchived:
		return // already versioned, or deliberately archived
	default:
		progress.Warnf("unexpected status %q checking %s; not seeding", res.Response.Status, docPath)
		return
	}

	content, err := seedFS.ReadFile(name)
	if err != nil {
		progress.Warnf("embedded seed %s unreadable: %v", name, err)
		return
	}
	// expected-version 0 = create-only: a concurrent writer winning the race is fine.
	pres, err := cl.Publish(ctx, fetch.WriteRequest{
		Host: seed.Host, Path: docPath, Token: seed.Token,
		Body: string(content), ExpectedVersion: 0, Metadata: seed.Meta,
	})
	if err != nil {
		progress.Warnf("could not seed %s: %v", docPath, err)
		return
	}
	switch pres.Response.Status {
	case protocol.StatusCreated, protocol.StatusOK:
		progress.Logf("seeded %s", docPath)
	case protocol.StatusConflict:
		// a concurrent writer created it; nothing to do
	default:
		progress.Warnf("could not seed %s: server returned %q", docPath, pres.Response.Status)
	}
}
