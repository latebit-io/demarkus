package graphstore

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// SeedSource is one owner's published graph as a surface reaches it.
type SeedSource struct {
	// Owner keys the gate, the etag and the seed: a dial host or a world name.
	Owner string
	// Fetch reads a path on the owner; ifNoneMatch is "" for shards.
	Fetch func(ctx context.Context, path, ifNoneMatch string) (protocol.Response, error)
	// Hub, when set, reads the hub that may hold the owner world's
	// checkpoint, preferred to the owner's own published graph.
	Hub func(ctx context.Context, path, ifNoneMatch string) (protocol.Response, error)
	// Rewrite adapts the rows before they become the owner's seed, and may drop
	// some: a gateway translates addresses and filters by tenant. Nil keeps them.
	Rewrite func(nodes []StoredNode, edges []StoredEdge) ([]StoredNode, []StoredEdge)
	// Problem hears why a pass failed; nil logs it.
	Problem func(SeedProblem)
}

// SeedProblem is why a seed pass did not complete.
type SeedProblem struct {
	Owner, Step, Path string
	Status            string // the unexpected status, when that was the problem
	Err               error
}

func (p SeedProblem) Error() string { //nolint:gocritic // a small report, formatted once
	if p.Err != nil {
		return fmt.Sprintf("graph seed %s %s on %s: %v", p.Step, p.Path, p.Owner, p.Err)
	}
	return fmt.Sprintf("graph seed %s %s on %s returned %s", p.Step, p.Path, p.Owner, p.Status)
}

// Seed refreshes the owner's seed at most once per SeedCheckInterval: its
// hub checkpoint first, then its snapshot, then its legacy export. A failed
// pass keeps the last good seed and marks it; the store is then saved.
func (s *Store) Seed(ctx context.Context, src SeedSource) { //nolint:gocritic // a source is built once and passed once
	s.seedChecks.Run(ctx, src.Owner, func(ctx context.Context) bool {
		replaced, problem := s.seedPass(ctx, &src)
		if problem != nil {
			s.MarkSeedFailure(src.Owner)
			src.report(problem)
		}
		// A current or absent graph changed nothing worth writing. A failed
		// save is its own problem: the mark or the new seed is lost on restart.
		if replaced || problem != nil {
			if err := s.Save(); err != nil {
				problem = &SeedProblem{Owner: src.Owner, Step: "save", Err: err}
				src.report(problem)
			}
		}
		return problem == nil
	})
}

// report hands a problem to the listener, or logs it: never silent.
func (src *SeedSource) report(problem *SeedProblem) {
	if src.Problem != nil {
		src.Problem(*problem)
		return
	}
	log.Printf("warning: %v", *problem)
}

// ExpireSeedCheck makes the owner's next Seed check again at once.
func (s *Store) ExpireSeedCheck(owner string) { s.seedChecks.Expire(owner) }

// LastSeedCheck is when the owner's published graph was last checked.
func (s *Store) LastSeedCheck(owner string) (time.Time, bool) { return s.seedChecks.LastCheck(owner) }

// seedPass has no problem when the published graph was current, refreshed or
// absent; replaced says the owner's seed is new.
func (s *Store) seedPass(ctx context.Context, src *SeedSource) (replaced bool, problem *SeedProblem) {
	etag := s.SeedEtag(src.Owner)
	failed := func(step, path string, resp protocol.Response, err error) (bool, *SeedProblem) {
		return false, &SeedProblem{Owner: src.Owner, Step: step, Path: path, Status: resp.Status, Err: err}
	}
	if src.Hub != nil {
		path := WorldManifestPath(src.Owner)
		manifest, err := src.Hub(ctx, path, etag)
		switch {
		case err != nil:
			return failed("fetch", path, manifest, err)
		case manifest.Status == protocol.StatusNotModified:
			return false, nil
		case manifest.Status != protocol.StatusNotFound:
			if err := s.seedCheckpoint(ctx, src, manifest); err != nil {
				return failed("load", path, manifest, err)
			}
			return true, nil
		}
	}
	manifest, err := src.Fetch(ctx, SnapshotManifestPath, etag)
	switch {
	case err != nil:
		return failed("fetch", SnapshotManifestPath, manifest, err)
	case manifest.Status == protocol.StatusNotModified:
		return false, nil
	case manifest.Status == protocol.StatusOK:
		nodes, edges, err := LoadSnapshot(SnapshotManifestPath, manifest, func(shardPath string) (protocol.Response, error) {
			return src.Fetch(ctx, shardPath, "")
		})
		if err != nil {
			return failed("load", SnapshotManifestPath, manifest, err)
		}
		s.commitSeed(src, &seedUpdate{nodes: nodes, edges: edges, etag: manifest.Metadata["etag"]})
		return true, nil
	case manifest.Status != protocol.StatusNotFound:
		return failed("fetch", SnapshotManifestPath, manifest, nil)
	}

	legacy, err := src.Fetch(ctx, LegacyExportPath, etag)
	switch {
	case err != nil:
		return failed("fetch", LegacyExportPath, legacy, err)
	case legacy.Status == protocol.StatusNotFound, legacy.Status == protocol.StatusNotModified:
		return false, nil
	case legacy.Status != protocol.StatusOK:
		return failed("fetch", LegacyExportPath, legacy, nil)
	}
	nodes, edges, err := ParseExportStrict(legacy.Body)
	if err != nil {
		return failed("parse", LegacyExportPath, legacy, err)
	}
	s.commitSeed(src, &seedUpdate{nodes: nodes, edges: edges, etag: legacy.Metadata["etag"]})
	return true, nil
}

// seedUpdate is one pass's new seed for its owner.
type seedUpdate struct {
	nodes []StoredNode
	edges []StoredEdge
	// keep carries records of the current seed over; nil replaces it whole.
	keep func(key string) bool
	// pins is the world checkpoint whose shards the seed then holds, or nil.
	pins *WorldManifest
	etag string
}

// commitSeed makes u the owner's seed, rewritten for the surface. The etag is
// recorded even empty: one left from another source would answer
// not-modified for that source while the seed holds this one.
func (s *Store) commitSeed(src *SeedSource, u *seedUpdate) {
	if src.Rewrite != nil {
		u.nodes, u.edges = src.Rewrite(u.nodes, u.edges)
	}
	s.replaceSeed(src.Owner, u)
	s.SetSeedEtag(src.Owner, u.etag)
}
