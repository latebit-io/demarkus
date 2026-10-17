package graph

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrIncomplete matches capped, cancelled, and failed crawl outcomes.
var ErrIncomplete = errors.New("crawl incomplete")

// Partial reasons accumulate; hitting requested depth is successful scope completion.
const (
	ReasonNodeCap      = "node-cap"
	ReasonFrontierCap  = "frontier-cap"
	ReasonByteCap      = "byte-cap"
	ReasonOutputCap    = "output-cap"
	ReasonCancelled    = "caller-cancelled"
	ReasonFetchFailure = "fetch-failure"
)

// CrawlOutcome covers a requested neighborhood, never a complete world inventory.
// Unknown coverage (a graph built from storage) has no outcome.
type CrawlOutcome struct {
	StartURL     string
	MaxDepth     int
	Complete     bool
	Reasons      []string
	Failures     int
	Fetches      int
	FetchedBytes int64 // decoded payloads accepted from fetchers, including cache hits
	ReadBytes    int64 // network response reads, including failed attempts
	Admitted     int
	PeakFrontier int
	PeakWorkers  int
	RejectedRels int // rel- metadata values that produced no edge
	cause        error
}

func (o *CrawlOutcome) addReason(reason string) {
	if !slices.Contains(o.Reasons, reason) {
		o.Reasons = append(o.Reasons, reason)
	}
}

func (o *CrawlOutcome) Error() string { return o.Summary() }
func (o *CrawlOutcome) Unwrap() error { return o.cause }

// Is preserves a stable sentinel while Unwrap retains caller cancellation.
func (o *CrawlOutcome) Is(target error) bool { return target == ErrIncomplete && !o.Complete }

// CrawlWarning removes an already-rendered outcome from joined persistence errors.
// Other errors retain their context and identity, including ordinary wrappers.
func CrawlWarning(err error, outcome *CrawlOutcome) error {
	if err == nil || err == outcome { //nolint:errorlint // identity only; wrappers keep their context
		return nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err
	}
	var remaining []error
	for _, cause := range joined.Unwrap() {
		if warning := CrawlWarning(cause, outcome); warning != nil {
			remaining = append(remaining, warning)
		}
	}
	return errors.Join(remaining...)
}

// Summary explicitly qualifies completeness by neighborhood depth.
func (o *CrawlOutcome) Summary() string {
	status := "complete"
	if !o.Complete {
		status = "partial"
	}
	s := fmt.Sprintf("scope: neighborhood; depth: %d; outcome: %s", o.MaxDepth, status)
	if len(o.Reasons) > 0 {
		s += "; reasons: " + strings.Join(o.Reasons, ", ")
	}
	if o.Failures > 0 {
		s += fmt.Sprintf("; failed: %d", o.Failures)
	}
	if o.RejectedRels > 0 {
		s += fmt.Sprintf("; rejected relations: %d", o.RejectedRels)
	}
	return s
}

func nodeSummary(n *Node) string {
	title := n.Title
	if title == "" {
		title = "(no title)"
	}
	status := n.Status
	if n.Incomplete {
		status = "partial"
	}
	row := fmt.Sprintf("  [%-9s] %-40s %q  %d links", status, n.URL, title, n.LinkCount)
	if n.Error != "" {
		row += fmt.Sprintf("; error: %q", n.Error)
	}
	return row + n.Observation.Annotation() + "\n"
}

func edgeSummary(e *Edge) string {
	return fmt.Sprintf("  %s -> %s%s\n", e.From, e.To, e.Annotation())
}

// Summary shares the graph output contract between MCP adapters.
func Summary(g *Graph, startURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Crawled %d nodes, %d edges from %s\n", g.NodeCount(), g.EdgeCount(), startURL)
	if g.Outcome != nil {
		b.WriteString(g.Outcome.Summary() + "\n")
	}
	nodes := g.AllNodes()
	slices.SortFunc(nodes, func(a, b *Node) int { return strings.Compare(a.URL, b.URL) })
	if len(nodes) > 0 {
		b.WriteString("\nNodes:\n")
		for _, n := range nodes {
			b.WriteString(nodeSummary(n))
		}
	}
	edges := g.GetEdges()
	if len(edges) > 0 {
		b.WriteString("\nEdges:\n")
		for i := range edges {
			b.WriteString(edgeSummary(&edges[i]))
		}
	}
	return b.String()
}
