// Package seed publishes the memory's first documents through the protocol
// and retires the flat files an older seeding left behind.
package seed

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/progress"
)

//go:embed index.md
var seedFS embed.FS

// pristineTemplateHashes are the sha256 of every project-template.md the old
// filesystem seeding ever shipped. The layout now lives in the remember
// skill; a flat copy matching one of these is an untouched seed, safe to delete.
var pristineTemplateHashes = map[string]bool{
	"2573bd505e8ed7f3573a2fd24e903b2dbf65df4dc03a933a8f5a10f63610c7a6": true,
	"e62bf567bf066d421ce7808cc7ee5077a2a6179b52b5b521fc25721b2960e109": true,
	"1e28177e16d4e580b34566a62d804809fe90c1ca188424ca51b20f4f9c351e93": true,
}

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

// CleanupLegacyTemplate deletes the old seeding's flat project-template.md
// (never FETCHable, issue #288) when pristine; the layout now ships in the
// remember skill. A customized copy stays until its owner republishes.
func CleanupLegacyTemplate(memoryDir string) {
	const name = "project-template.md"
	flatPath := filepath.Join(memoryDir, name)
	fi, err := os.Lstat(flatPath)
	if err != nil {
		if !os.IsNotExist(err) {
			progress.Warnf("could not stat %s: %v", flatPath, err)
		}
		return
	}
	if !fi.Mode().IsRegular() {
		return // a symlink is an already-versioned doc
	}
	flat, err := os.ReadFile(flatPath)
	if err != nil {
		progress.Warnf("could not read flat %s: %v", name, err)
		return
	}
	sum := sha256.Sum256(flat)
	if !pristineTemplateHashes[hex.EncodeToString(sum[:])] {
		return
	}
	if err := os.Remove(flatPath); err != nil {
		progress.Warnf("could not remove legacy flat %s: %v", name, err)
		return
	}
	progress.Logf("removed legacy flat %s (the layout now ships in the remember skill)", name)
}

// seedDoc publishes the embedded seed for name at the memory root unless the
// server already serves it. Create-only: over a legacy flat file this relies
// on the store's flat-to-v1 migration, whose conflict preserves user content.
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
