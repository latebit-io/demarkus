package graphstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/graph"
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

// Revalidate reads due sources without following edges. Nil URLs means all sources;
// an empty non-nil list means no sources. Attempts are single-flight and cooled down.
func (s *Store) Revalidate(ctx context.Context, urls []string, fetchFn FetchFunc, parseURL func(string) (string, string, error)) (Revalidation, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	wanted := make(map[string]bool, len(urls))
	for _, url := range urls {
		wanted[url] = true
	}
	nodes := s.AllNodes()
	slices.SortFunc(nodes, func(a, b StoredNode) int {
		if order := a.Observation.AttemptedAt.Compare(b.Observation.AttemptedAt); order != 0 {
			return order
		}
		return strings.Compare(a.URL, b.URL)
	})
	var result Revalidation
	var failures []error
	for i := range nodes {
		node := &nodes[i]
		if (urls != nil && !wanted[node.URL]) || !strings.HasPrefix(node.URL, "mark://") || !dueObservation(node, time.Now()) {
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
		crawled, err := s.CrawlAndPersist(ctx, node.URL, fetchFn, parseURL, CrawlOptions{
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
	}
	if ctx.Err() != nil {
		failures = append(failures, ctx.Err())
	}
	return result, errors.Join(failures...)
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
