package main

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/client/internal/graph"
)

// viewMode distinguishes between document reading and graph exploration.
type viewMode int

const (
	viewDocument viewMode = iota
	viewGraph
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
	url    string
	title  string
	status string
	depth  int
}

// startCrawl returns a tea.Cmd that crawls outbound links from url.
func (m model) startCrawl(url string) tea.Cmd {
	seq := m.crawlSeq
	client := m.client
	return func() tea.Msg {
		fetcher := &graph.ClientFetcher{
			FetchFunc: func(host, path string) (string, string, error) {
				r, err := client.Fetch(host, path)
				if err != nil {
					return "", "", err
				}
				return r.Response.Status, r.Response.Body, nil
			},
		}
		g, err := graph.Crawl(context.Background(), url, fetcher, fetch.ParseMarkURL, graph.CrawlOptions{
			MaxDepth: 10,
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
			url:    n.URL,
			title:  n.Title,
			status: n.Status,
			depth:  n.Depth,
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

		line := fmt.Sprintf("%s%s%s%s %s", cursor, indent, connector, icon, label)

		// Truncate to width.
		if len(line) > width-2 {
			line = line[:width-5] + "..."
		}

		b.WriteString(line)
		b.WriteByte('\n')
	}

	b.WriteString("\n  [Enter] navigate  [d] back to document  [q] quit\n")
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

// handleGraphKey processes key events when the graph view is active.
func (m model) handleGraphKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "d", "esc":
		m.viewMode = viewDocument
		if m.histIdx >= 0 {
			m.restoreHistory()
		}
		return m, nil
	case "j", "down":
		if m.graphIdx < len(m.graphNodes)-1 {
			m.graphIdx++
			if m.ready {
				m.viewport.SetContent(renderGraphView(m.graphNodes, m.graphIdx, m.width))
			}
		}
		return m, nil
	case "k", "up":
		if m.graphIdx > 0 {
			m.graphIdx--
			if m.ready {
				m.viewport.SetContent(renderGraphView(m.graphNodes, m.graphIdx, m.width))
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
