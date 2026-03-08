package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/client/internal/graph"
	"github.com/latebit/demarkus/client/internal/graphstore"
)

// viewMode distinguishes between document reading and graph exploration.
type viewMode int

const (
	viewDocument viewMode = iota
	viewGraph
)

// graphSubView selects which data the graph view displays.
type graphSubView int

const (
	subViewLinks     graphSubView = iota // BFS tree from current doc (default)
	subViewBacklinks                     // documents linking TO current doc
	subViewTopology                      // all explored nodes by importance
)

// crawlResult is sent when the async graph crawl completes.
type crawlResult struct {
	graph *graph.Graph
	err   error
	url   string
	seq   uint64
}

// graphListItem is a flattened node for display in the tree view.
type graphListItem struct {
	url       string
	title     string
	status    string
	depth     int
	backlinks int // inbound link count from the graph
}

// maxCrawlNodes caps the number of documents crawled to prevent runaway graphs.
const maxCrawlNodes = 200

// startCrawl returns a tea.Cmd that crawls outbound links from url.
func (m model) startCrawl(url string) tea.Cmd {
	seq := m.crawlSeq
	client := m.client
	gs := m.graphStore
	return func() tea.Msg {
		g, err := gs.CrawlAndPersist(context.Background(), url, func(host, path string) (string, string, string, error) {
			r, fetchErr := client.Fetch(host, path)
			if fetchErr != nil {
				return "", "", "", fetchErr
			}
			return r.Response.Status, r.Response.Body, r.Response.Metadata["etag"], nil
		}, fetch.ParseMarkURL, graphstore.CrawlOptions{
			MaxDepth: 10,
			MaxNodes: maxCrawlNodes,
			Workers:  5,
		})
		return crawlResult{graph: g, err: err, url: url, seq: seq}
	}
}

// flattenGraph builds a display list from the graph using BFS from rootURL.
// The root appears first, followed by its neighbors, then their neighbors, etc.
func flattenGraph(g *graph.Graph, rootURL string) []graphListItem {
	if g == nil {
		return nil
	}
	root := g.GetNode(rootURL)
	if root == nil {
		return nil
	}

	var items []graphListItem
	visited := map[string]bool{rootURL: true}
	queue := []string{rootURL}

	for len(queue) > 0 {
		url := queue[0]
		queue = queue[1:]

		n := g.GetNode(url)
		if n == nil {
			continue
		}

		items = append(items, graphListItem{
			url:       n.URL,
			title:     n.Title,
			status:    n.Status,
			depth:     n.Depth,
			backlinks: len(g.InNeighbors(n.URL)),
		})

		for _, neighbor := range g.Neighbors(url) {
			if !visited[neighbor.URL] {
				visited[neighbor.URL] = true
				queue = append(queue, neighbor.URL)
			}
		}
	}

	return items
}

// backlinksList returns a flat list of documents linking to url from the store.
func backlinksList(gs *graphstore.Store, url string) []graphListItem {
	if gs == nil {
		return nil
	}
	entries := gs.BacklinksEnriched(url)
	items := make([]graphListItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, graphListItem{
			url:    e.URL,
			title:  e.Title,
			status: e.Status,
		})
	}
	return items
}

// topologyList returns all nodes from the store sorted by backlink count descending.
func topologyList(gs *graphstore.Store) []graphListItem {
	if gs == nil {
		return nil
	}
	nodes := gs.AllNodes()
	degrees := gs.InDegrees()
	items := make([]graphListItem, 0, len(nodes))
	for _, n := range nodes {
		items = append(items, graphListItem{
			url:       n.URL,
			title:     n.Title,
			status:    n.Status,
			backlinks: degrees[n.URL],
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].backlinks != items[j].backlinks {
			return items[i].backlinks > items[j].backlinks
		}
		return items[i].url < items[j].url
	})
	return items
}

// renderGraphView renders the tree list as a string for the viewport.
func renderGraphView(items []graphListItem, selectedIdx, width int) string {
	if len(items) == 0 {
		return "\n  No nodes discovered.\n"
	}

	var b strings.Builder
	b.WriteString("\n  Document Graph\n\n")

	for i, item := range items {
		label := item.title
		if label == "" {
			label = item.url
		}

		icon := statusIcon(item.status)

		// Indentation: 4 spaces per depth level.
		indent := strings.Repeat("    ", item.depth)

		// Tree connector.
		connector := ""
		if item.depth > 0 {
			connector = "├─ "
		}

		cursor := "  "
		if i == selectedIdx {
			cursor = "> "
		}

		// Backlink density indicator.
		density := ""
		if item.backlinks > 0 {
			density = fmt.Sprintf(" [%d←]", item.backlinks)
		}

		line := fmt.Sprintf("%s%s%s%s %s%s", cursor, indent, connector, icon, label, density)

		// Truncate to width.
		if width > 5 && len(line) > width-2 {
			line = line[:width-5] + "..."
		}

		b.WriteString(line)
		b.WriteByte('\n')
	}

	b.WriteString("\n  [Enter] navigate  [r] backlinks  [t] topology  [d] close  [q] quit\n")
	return b.String()
}

func statusIcon(status string) string {
	switch status {
	case "ok":
		return "●"
	case "not-found":
		return "○"
	case "error":
		return "✗"
	case "external":
		return "→"
	default:
		return "○"
	}
}

// renderBacklinksView renders the backlinks list for the viewport.
func renderBacklinksView(items []graphListItem, selectedIdx, width int) string {
	var b strings.Builder
	b.WriteString("\n  Backlinks\n\n")

	if len(items) == 0 {
		b.WriteString("  No backlinks found.\n  Run a graph crawl (d) to populate the store.\n")
		b.WriteString("\n  [d] links  [t] topology  [Esc] close  [q] quit\n")
		return b.String()
	}

	for i, item := range items {
		label := item.title
		if label == "" {
			label = item.url
		}
		icon := statusIcon(item.status)

		cursor := "  "
		if i == selectedIdx {
			cursor = "> "
		}

		line := fmt.Sprintf("%s%s %s", cursor, icon, label)
		if width > 5 && len(line) > width-2 {
			line = line[:width-5] + "..."
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}

	b.WriteString("\n  [Enter] navigate  [d] links  [t] topology  [Esc] close  [q] quit\n")
	return b.String()
}

// renderTopologyView renders all explored nodes sorted by importance.
func renderTopologyView(items []graphListItem, selectedIdx, width int) string {
	var b strings.Builder
	b.WriteString("\n  Topology (all explored nodes)\n\n")

	if len(items) == 0 {
		b.WriteString("  No nodes in graph store.\n  Run a graph crawl (d) to populate.\n")
		b.WriteString("\n  [d] links  [r] backlinks  [Esc] close  [q] quit\n")
		return b.String()
	}

	for i, item := range items {
		label := item.title
		if label == "" {
			label = item.url
		}
		icon := statusIcon(item.status)

		density := ""
		if item.backlinks > 0 {
			density = fmt.Sprintf(" [%d←]", item.backlinks)
		}

		cursor := "  "
		if i == selectedIdx {
			cursor = "> "
		}

		line := fmt.Sprintf("%s%s %s%s", cursor, icon, label, density)
		if width > 5 && len(line) > width-2 {
			line = line[:width-5] + "..."
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}

	b.WriteString("\n  [Enter] navigate  [d] links  [r] backlinks  [Esc] close  [q] quit\n")
	return b.String()
}

// handleGraphKey processes key events when the graph view is active.
func (m model) handleGraphKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "esc":
		m.viewMode = viewDocument
		if m.histIdx >= 0 {
			m.restoreHistory()
		} else if m.ready {
			m.viewport.SetContent("\n  No document loaded.\n  Use the address bar to load a document.\n")
		}
		return m, nil
	case "d":
		if m.graphSubView != subViewLinks {
			m.graphSubView = subViewLinks
			url := m.addressBar.Value()
			m.graphNodes = flattenGraph(m.graphData, url)
			m.graphIdx = 0
			if m.ready {
				m.viewport.SetContent(renderGraphView(m.graphNodes, m.graphIdx, m.width))
				m.viewport.GotoTop()
			}
		} else {
			m.viewMode = viewDocument
			if m.histIdx >= 0 {
				m.restoreHistory()
			} else if m.ready {
				m.viewport.SetContent("\n  No document loaded.\n  Use the address bar to load a document.\n")
			}
		}
		return m, nil
	case "r":
		m.graphSubView = subViewBacklinks
		url := m.addressBar.Value()
		m.graphNodes = backlinksList(m.graphStore, url)
		m.graphIdx = 0
		if m.ready {
			m.viewport.SetContent(renderBacklinksView(m.graphNodes, m.graphIdx, m.width))
			m.viewport.GotoTop()
		}
		return m, nil
	case "t":
		m.graphSubView = subViewTopology
		m.graphNodes = topologyList(m.graphStore)
		m.graphIdx = 0
		if m.ready {
			m.viewport.SetContent(renderTopologyView(m.graphNodes, m.graphIdx, m.width))
			m.viewport.GotoTop()
		}
		return m, nil
	case "j", "down":
		if m.graphIdx < len(m.graphNodes)-1 {
			m.graphIdx++
			if m.ready {
				m.viewport.SetContent(m.renderCurrentGraphSubView())
			}
		}
		return m, nil
	case "k", "up":
		if m.graphIdx > 0 {
			m.graphIdx--
			if m.ready {
				m.viewport.SetContent(m.renderCurrentGraphSubView())
			}
		}
		return m, nil
	case "enter":
		if m.graphIdx >= 0 && m.graphIdx < len(m.graphNodes) {
			target := m.graphNodes[m.graphIdx].url
			m.viewMode = viewDocument
			m.addressBar.SetValue(target)
			m.loading = true
			m.fetchSeq++
			return m, m.doFetch(target)
		}
		return m, nil
	}
	return m, nil
}

// renderCurrentGraphSubView returns the rendered content for the active sub-view.
func (m model) renderCurrentGraphSubView() string {
	switch m.graphSubView {
	case subViewBacklinks:
		return renderBacklinksView(m.graphNodes, m.graphIdx, m.width)
	case subViewTopology:
		return renderTopologyView(m.graphNodes, m.graphIdx, m.width)
	default:
		return renderGraphView(m.graphNodes, m.graphIdx, m.width)
	}
}
