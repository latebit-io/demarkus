package marktools

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/provenance"
)

// maxDriftReads bounds the sources one fetch reads to judge its pins.
const maxDriftReads = 5

// driftLine words the pinned sources on this store a fetched document has
// fallen behind, "" when none did. Sources are read at fetch time, at once,
// never stored; other stores, repositories and the web are not read.
func (t *Tools) driftLine(ctx context.Context, target Target, token string, meta map[string]string) string {
	type source struct {
		path   string
		pinned int
	}
	var sources []source
	// A version path (/doc.md/vN) is read too; its pins resolve from the document.
	doc := provenance.ParsePin(target.Path).Source
	resolve, seen := links.ResolverFor(target.Host), map[string]bool{}
	for _, pin := range provenance.Parse(meta[provenance.Key]) {
		pinned := pin.DocumentVersion()
		if pinned == 0 {
			continue
		}
		if p := resolve.Path(doc, pin.Source); p != "" && p != doc && !seen[p] {
			seen[p] = true
			sources = append(sources, source{p, pinned})
		}
	}
	over := max(len(sources)-maxDriftReads, 0)
	sources = sources[:len(sources)-over]
	drifts, read := make([]string, len(sources)), make([]bool, len(sources))
	var wg sync.WaitGroup
	for i, s := range sources {
		wg.Go(func() {
			drifts[i], read[i] = t.sourceDrift(ctx, fetch.FetchRequest{Host: target.Host, Path: s.path, Token: token}, s.pinned)
		})
	}
	wg.Wait()
	var parts []string
	unreadable := 0
	for i, d := range drifts {
		switch {
		case !read[i]:
			unreadable++
		case d != "":
			parts = append(parts, d)
		}
	}
	stale := len(parts) > 0
	if unreadable > 0 {
		parts = append(parts, fmt.Sprintf("%d unreadable", unreadable))
	}
	if over > 0 {
		parts = append(parts, fmt.Sprintf("%d past the limit of %d", over, maxDriftReads))
	}
	if stale {
		parts = append(parts, "may be stale")
	}
	return strings.Join(parts, "; ")
}

// sourceDrift reads one source and words where it stands against the pin,
// "" when current; read is false when it could not be judged.
func (t *Tools) sourceDrift(ctx context.Context, r fetch.FetchRequest, pinned int) (drift string, read bool) {
	result, err := t.backend.Fetch(ctx, r)
	status := result.Response.Status
	// An absent version is 0; the switch counts it and a malformed one unreadable.
	current, malformed := docwrite.Version(result.Response.Metadata)
	switch {
	case err != nil:
		// The line's count surfaces it; the document itself was read.
		return "", false
	case status == protocol.StatusNotFound, status == protocol.StatusArchived:
		return r.Path + " is not a live document here", true
	case status != protocol.StatusOK, malformed != nil, current == 0:
		return "", false
	}
	if problem, _ := provenance.Drift(pinned, current); problem != "" {
		return r.Path + " " + problem, true
	}
	return "", true
}
