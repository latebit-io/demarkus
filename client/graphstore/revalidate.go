package graphstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/links"
)

// RevalidationLimit bounds source attempts, including pre-fetch failures.
const RevalidationLimit = 8
const revalidationBytes int64 = 8 << 20

// Revalidation reports source work separately from graph traversal.
type Revalidation struct {
	Attempts  int
	Fetches   int
	Bytes     int64
	ReadBytes int64
	Remaining int
}

// Summary exposes validation cost and unvalidated remainder.
func (r Revalidation) Summary() string {
	return fmt.Sprintf("revalidation: %d attempts, %d fetches, %d decoded bytes, %d read bytes; %d due sources remain", r.Attempts, r.Fetches, r.Bytes, r.ReadBytes, r.Remaining)
}

// ValidationSummary keeps failures beside retained evidence on every surface.
func ValidationSummary(result Revalidation, err error) string {
	text := ""
	if result.Attempts > 0 || result.Remaining > 0 {
		text = result.Summary() + "\n"
	}
	if err != nil {
		text += fmt.Sprintf("warning: %v\n", err)
	}
	return text
}

// RevalidateBacklinks refreshes the sources linking to url and reports the
// pass; an empty string means nothing links there. Shared by every surface.
func (s *Store) RevalidateBacklinks(ctx context.Context, url string, fetchFn FetchFunc) string {
	urls := s.Backlinks(url)
	if len(urls) == 0 {
		return ""
	}
	result, err := s.Revalidate(ctx, urls, fetchFn)
	return ValidationSummary(result, err) + s.FreshnessSummaryFor(urls) + "\n"
}

// Revalidate reads due sources without following edges. Nil URLs means all sources;
// an empty non-nil list means no sources. Attempts are single-flight and cooled down.
// The pass merges and saves once, so the store rebuilds once per call.
func (s *Store) Revalidate(ctx context.Context, urls []string, fetchFn FetchFunc) (Revalidation, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	nodes := s.nodesFor(urls)
	slices.SortFunc(nodes, func(a, b StoredNode) int {
		if order := a.Observation.AttemptedAt.Compare(b.Observation.AttemptedAt); order != 0 {
			return order
		}
		return strings.Compare(a.URL, b.URL)
	})
	var result Revalidation
	var failures []error
	observed := graph.New()
	etags := make(map[string]string)
	for i := range nodes {
		node := &nodes[i]
		if !strings.HasPrefix(node.URL, "mark://") || !dueObservation(node, time.Now()) {
			continue
		}
		if result.Attempts >= RevalidationLimit || max(result.Bytes, result.ReadBytes) >= revalidationBytes || ctx.Err() != nil {
			result.Remaining++
			continue
		}
		if !s.claimValidation(node.URL) {
			continue
		}
		result.Attempts++
		crawled, crawledEtags, err := crawlGraph(ctx, node.URL, fetchFn, CrawlOptions{
			MaxDepth: 0, MaxNodes: 1, Workers: 1,
			MaxFetchBytes: revalidationBytes - max(result.Bytes, result.ReadBytes), MaxOutputBytes: 128 << 10,
		})
		s.finishValidation(node.URL)
		if crawled != nil && crawled.Outcome != nil {
			result.Fetches += crawled.Outcome.Fetches
			result.Bytes += crawled.Outcome.FetchedBytes
			result.ReadBytes += crawled.Outcome.ReadBytes
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("revalidate %s: %w", node.URL, err))
		}
		if crawled != nil && (err == nil || errors.Is(err, graph.ErrIncomplete)) {
			addGraph(observed, crawled)
			maps.Copy(etags, crawledEtags)
		}
	}
	if ctx.Err() != nil {
		failures = append(failures, ctx.Err())
	}
	if observed.NodeCount() > 0 {
		s.Merge(observed, etags)
		if saveErr := s.Save(); saveErr != nil {
			failures = append(failures, fmt.Errorf("revalidation save: %w", saveErr))
		}
	}
	return result, errors.Join(failures...)
}

// nodesFor copies the requested sources, or every node when urls is nil.
func (s *Store) nodesFor(urls []string) []StoredNode {
	if urls == nil {
		return s.AllNodes()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	nodes := make([]StoredNode, 0, len(urls))
	for _, url := range urls {
		if node := s.nodes[links.CanonicalURL(url)]; node != nil {
			nodes = append(nodes, *node)
		}
	}
	return nodes
}

// addGraph folds src into dst; depth-0 crawls of distinct sources never conflict.
func addGraph(dst, src *graph.Graph) {
	for _, node := range src.AllNodes() {
		dst.AddNode(node)
	}
	for _, edge := range src.GetEdges() {
		dst.AddEdgeInfo(edge)
	}
}

func (s *Store) claimValidation(url string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.validating == nil {
		s.validating = make(map[string]time.Time)
	}
	if last, ok := s.validating[url]; ok && time.Since(last) < graph.FreshnessWindow {
		return false
	}
	s.validating[url] = time.Now()
	return true
}

func (s *Store) finishValidation(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.validating[url] = time.Now()
}
