package graphstore

import (
	"context"
	"time"

	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

// worldRows turns a world's checkpoint rows into graph rows observed at
// observed: each source is the node mark://<world><path> at its version.
func worldRows(world string, sources []WorldSource, observed time.Time) ([]StoredNode, []StoredEdge) {
	count := 0
	for i := range sources {
		count += len(sources[i].Edges)
	}
	nodes, edges := make([]StoredNode, 0, len(sources)), make([]StoredEdge, 0, count)
	for i := range sources {
		src := &sources[i]
		url := links.NodeURL(world, src.Path)
		nodes = append(nodes, StoredNode{
			URL: url, Title: src.Title, Status: protocol.StatusOK, LinkCount: src.LinkCount(), Etag: src.Etag,
			Observation: graph.Observation{
				Source: url, View: graph.ViewFederation, Revision: src.Version, Etag: src.Etag,
				Complete: true, ObservedAt: observed, AttemptedAt: observed,
			},
		})
		for _, edge := range src.Edges {
			edges = append(edges, StoredEdge{From: url, To: edge.To, Rel: edge.Rel, Label: edge.Label, Anchor: edge.Anchor, Count: edge.Count})
		}
	}
	return nodes, edges
}

// seedCheckpoint seeds the owner world from its checkpoint manifest in the
// hub. Over a seed that came from the previous checkpoint, only shards whose
// pins changed are fetched and replaced.
func (s *Store) seedCheckpoint(ctx context.Context, src *SeedSource, manifest protocol.Response) error {
	s.mu.RLock()
	previous := s.worldPins[src.Owner]
	s.mu.RUnlock()
	load, err := LoadWorld(ctx, WorldLoadRequest{World: src.Owner, Manifest: manifest, Previous: previous, Fetch: func(ctx context.Context, shard string) (protocol.Response, error) {
		return src.Hub(ctx, shard, "")
	}})
	if err != nil {
		return err
	}
	m := &load.Manifest
	update := &seedUpdate{pins: m, etag: manifest.Metadata["etag"]}
	if previous != nil {
		if len(load.Kept) == len(m.Shards) && len(previous.Shards) == len(m.Shards) && previous.Complete == m.Complete {
			// Only the cursor moved: no row changed, so no rebuild.
			s.mu.Lock()
			s.setPinsLocked(src.Owner, m)
			s.mu.Unlock()
			s.SetSeedEtag(src.Owner, update.etag)
			return nil
		}
		// An incomplete checkpoint is a subset: what it lacks is not removed.
		authority := links.AuthorityURL(src.Owner)
		update.keep = func(key string) bool {
			target, err := links.ParseMark(key)
			return err == nil && target.AuthorityURL() == authority && (!m.Complete || load.Kept[SourcePrefix(target.Path, m.PrefixLength)])
		}
	}
	update.nodes, update.edges = worldRows(src.Owner, load.Sources, time.Now())
	s.commitSeed(src, update)
	return nil
}
