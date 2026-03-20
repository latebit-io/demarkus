package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit/demarkus/client/internal/bookmarks"
	"github.com/latebit/demarkus/client/internal/cache"
	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/client/internal/graph"
	"github.com/latebit/demarkus/client/internal/graphstore"
	"github.com/latebit/demarkus/client/internal/links"
	"github.com/latebit/demarkus/client/internal/tokens"
	"github.com/latebit/demarkus/protocol"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

type focus int

const (
	focusAddressBar focus = iota
	focusViewport
)

// historyEntry stores a snapshot of a visited page for instant back/forward.
type historyEntry struct {
	url            string
	rendered       string // glamour-rendered content (cleaned, no markers)
	markedRendered string // glamour output with markers for re-highlighting
	rawBody        string
	status         string
	metadata       map[string]string
	links          []string         // resolved absolute mark:// URLs
	linkInfos      []links.LinkInfo // link positions for marker injection
}

type model struct {
	addressBar  textinput.Model
	viewport    viewport.Model
	focus       focus
	status      string
	metadata    map[string]string
	fromCache   bool
	err         error
	loading     bool
	client      *fetch.Client
	pendingBody string
	width       int
	height      int
	ready       bool

	// Cached markdown renderer (re-created only when width changes).
	renderer      *glamour.TermRenderer
	rendererWidth int

	// History navigation
	history []historyEntry
	histIdx int

	// Link navigation
	rawBody        string           // raw markdown body of current page
	links          []string         // resolved absolute mark:// URLs
	linkIdx        int              // -1 = none selected
	linkInfos      []links.LinkInfo // extended link info with source positions
	linkRegions    []linkRegion     // rendered coordinate map for click detection
	markedRendered string           // glamour output with markers (before highlight)
	hoverIdx       int              // -1 = no hover, index of link under mouse cursor

	// Fetch sequencing: ignore stale results from superseded fetches.
	fetchSeq uint64

	// Graph view
	viewMode     viewMode
	graphSubView graphSubView
	graphData    *graph.Graph
	graphNodes   []graphListItem
	graphIdx     int
	crawling     bool
	crawlSeq     uint64

	showHelp bool

	// Bookmarks
	bookmarkStore *bookmarks.Store
	bookmarkMsg   string // transient status message
	bookmarkSeq   uint64 // sequence counter for stale clear prevention

	// Persistent graph
	graphStore *graphstore.Store

	// Terminal style — "dark" or "light", resolved once before Bubbletea starts.
	styleName string
}

type fetchResult struct {
	result fetch.Result
	err    error
	url    string
	seq    uint64
}

// clearBookmarkMsg signals the transient bookmark message should be cleared.
// seq must match bookmarkSeq to avoid stale clears from rapid toggling.
type clearBookmarkMsg struct{ seq uint64 }

// viewportReady is sent after the viewport is created to process any
// pending content in a separate update cycle, avoiding a rendering
// issue where SetContent during viewport creation doesn't display.
type viewportReady struct{}

// pushHistory appends entry to history after histIdx, truncating forward entries.
// Caps at 50 entries; returns updated (history, histIdx).
func pushHistory(history []historyEntry, idx int, entry historyEntry) (updated []historyEntry, newIdx int) {
	// Truncate forward entries (everything after idx).
	history = history[:idx+1]
	history = append(history, entry)
	// Cap at 50: drop oldest.
	if len(history) > 50 {
		history = history[len(history)-50:]
	}
	return history, len(history) - 1
}

// canGoBack reports whether backward navigation is possible.
func (m model) canGoBack() bool {
	return m.histIdx > 0
}

// canGoForward reports whether forward navigation is possible.
func (m model) canGoForward() bool {
	return m.histIdx < len(m.history)-1
}

// restoreHistory applies the current history entry to the model state.
// No network fetch — content is restored from the snapshot.
func (m *model) restoreHistory() {
	entry := m.history[m.histIdx]
	m.addressBar.SetValue(entry.url)
	m.status = entry.status
	m.metadata = entry.metadata
	m.rawBody = entry.rawBody
	m.links = entry.links
	m.linkInfos = entry.linkInfos
	m.markedRendered = entry.markedRendered
	m.linkIdx = -1
	m.hoverIdx = -1
	m.err = nil
	m.loading = false
	m.fromCache = false
	if m.ready {
		content := entry.rendered
		if content == "" && entry.rawBody != "" {
			marked := injectLinkMarkers(entry.rawBody, entry.linkInfos)
			r, err := m.renderMarkdown(marked)
			if err != nil {
				content = entry.rawBody
			} else {
				m.markedRendered = r
				cleaned, regions := processMarkers(r, m.linkIdx, m.hoverIdx)
				m.linkRegions = regions
				content = cleaned
			}
			m.history[m.histIdx].rendered = content
			m.history[m.histIdx].markedRendered = m.markedRendered
		} else if m.markedRendered != "" {
			cleaned, regions := processMarkers(m.markedRendered, m.linkIdx, m.hoverIdx)
			m.linkRegions = regions
			content = cleaned
		}
		m.viewport.SetContent(content)
		m.viewport.GotoTop()
	}
}

const helpText = `
  Keyboard Shortcuts

  Navigation
    Enter        Follow selected link / fetch URL
    [ / Alt+Left   Go back
    ] / Alt+Right  Go forward
    Tab          Cycle through links on page
    d            Document graph view
    f            Focus address bar

  Bookmarks
    b            Toggle bookmark for current page
    B            View all bookmarks

  Scrolling
    j / Down     Scroll down
    k / Up       Scroll up
    g            Go to top
    G            Go to bottom

  General
    ?            Toggle this help screen
    q / Ctrl+C   Quit
    Esc          Exit bookmarks / dismiss help / blur address bar
`

func initialModel(initialURL string, client *fetch.Client, styleName string) model {
	ti := textinput.New()
	ti.Placeholder = "mark://host:port/path"
	ti.Prompt = " "
	ti.SetValue(initialURL)
	ti.Focus()

	bs, err := bookmarks.Load(bookmarks.DefaultPath())
	var bmMsg string
	if err != nil {
		bmMsg = "Failed to load bookmarks: " + err.Error()
	}

	gs, gsErr := graphstore.Load(graphstore.DefaultPath())
	if gsErr != nil {
		msg := "Failed to load graph store: " + gsErr.Error()
		if bmMsg != "" {
			bmMsg += " | " + msg
		} else {
			bmMsg = msg
		}
	}

	return model{
		addressBar:    ti,
		focus:         focusAddressBar,
		client:        client,
		loading:       initialURL != "",
		histIdx:       -1,
		linkIdx:       -1,
		hoverIdx:      -1,
		bookmarkStore: bs,
		bookmarkMsg:   bmMsg,
		graphStore:    gs,
		styleName:     styleName,
	}
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink}
	if m.addressBar.Value() != "" {
		cmds = append(cmds, m.doFetch(m.addressBar.Value()))
	}
	if m.bookmarkMsg != "" {
		cmds = append(cmds, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
			return clearBookmarkMsg{seq: 0}
		}))
	}
	return tea.Batch(cmds...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case tea.WindowSizeMsg:
		return m.handleWindowSize(msg)
	case crawlResult:
		return m.handleCrawlResult(msg)
	case fetchResult:
		return m.handleFetchResult(msg)
	case viewportReady:
		return m.handleViewportReady()
	case clearBookmarkMsg:
		if msg.seq == m.bookmarkSeq {
			m.bookmarkMsg = ""
		}
		return m, nil
	}
	return m, nil
}

func (m model) handleMouseHover(msg tea.MouseMsg) model {
	if m.markedRendered == "" || m.showHelp || m.err != nil || m.status == "bookmarks" {
		return m
	}
	newHover := -1
	if msg.Y >= 2 {
		contentLine := msg.Y - 2 + m.viewport.YOffset
		contentCol := msg.X
		for _, r := range m.linkRegions {
			if r.line == contentLine && contentCol >= r.startCol && contentCol < r.endCol {
				newHover = r.idx
				break
			}
		}
	}
	if newHover != m.hoverIdx {
		m.hoverIdx = newHover
		cleaned, regions := processMarkers(m.markedRendered, m.linkIdx, m.hoverIdx)
		m.linkRegions = regions
		m.viewport.SetContent(cleaned)
	}
	return m
}

func (m model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action == tea.MouseActionMotion && m.ready && m.viewMode == viewDocument {
		m = m.handleMouseHover(msg)
	}

	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		if msg.Y == 0 {
			m.focus = focusAddressBar
			m.addressBar.Focus()
			return m, textinput.Blink
		}
		if msg.Y >= 2 && m.ready && m.viewMode == viewDocument {
			// Check if click lands on a link.
			contentLine := msg.Y - 2 + m.viewport.YOffset
			contentCol := msg.X
			for _, r := range m.linkRegions {
				if r.line != contentLine || contentCol < r.startCol || contentCol >= r.endCol {
					continue
				}
				target := m.links[r.idx]
				m.addressBar.SetValue(target)
				m.loading = true
				m.fetchSeq++
				m.links = nil
				m.linkIdx = -1
				m.hoverIdx = -1
				return m, m.doFetch(target)
			}
			m.focus = focusViewport
			m.addressBar.Blur()
		} else if msg.Y >= 2 {
			m.focus = focusViewport
			m.addressBar.Blur()
		}
	}
	if m.ready {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) handleWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width = msg.Width
	m.height = msg.Height
	headerHeight := 2 // address bar + divider
	footerHeight := 1 // status bar
	viewportHeight := max(m.height-headerHeight-footerHeight, 1)

	if !m.ready {
		m.viewport = viewport.New(m.width, viewportHeight)
		m.ready = true
		// Defer pending content to a separate update cycle so the
		// viewport has a chance to fully initialise before receiving
		// content. Setting content in the same cycle as creation can
		// leave the viewport blank until the next event (e.g. scroll).
		if m.pendingBody != "" || m.err != nil {
			m.addressBar.Width = m.width - 2
			return m, func() tea.Msg { return viewportReady{} }
		}
	} else {
		m.viewport.Width = m.width
		m.viewport.Height = viewportHeight
		// Re-render graph view with new width for correct truncation.
		if m.viewMode == viewGraph && len(m.graphNodes) > 0 {
			m.viewport.SetContent(m.renderCurrentGraphSubView())
		}
	}
	m.addressBar.Width = m.width - 2
	return m, nil
}

func (m model) handleViewportReady() (tea.Model, tea.Cmd) {
	if m.pendingBody != "" {
		marked := injectLinkMarkers(m.pendingBody, m.linkInfos)
		rendered, err := m.renderMarkdown(marked)
		if err != nil {
			m.viewport.SetContent(m.pendingBody)
		} else {
			m.markedRendered = rendered
			cleaned, regions := processMarkers(rendered, m.linkIdx, m.hoverIdx)
			m.linkRegions = regions
			m.viewport.SetContent(cleaned)
		}
		m.pendingBody = ""
		m.viewport.GotoTop()
	} else if m.err != nil {
		m.viewport.SetContent(errorView(m.err))
		m.viewport.GotoTop()
	}
	return m, nil
}

func (m model) handleCrawlResult(msg crawlResult) (tea.Model, tea.Cmd) {
	if msg.seq != m.crawlSeq {
		return m, nil
	}
	m.crawling = false
	if msg.err != nil {
		m.viewMode = viewDocument
		m.err = msg.err
		if m.ready {
			m.viewport.SetContent(errorView(msg.err))
		}
		return m, nil
	}
	m.graphData = msg.graph

	// Recompute display list for the active sub-view.
	switch m.graphSubView {
	case subViewBacklinks:
		m.graphNodes = backlinksList(m.graphStore, msg.url)
	case subViewTopology:
		m.graphNodes = topologyList(m.graphStore)
	default:
		m.graphNodes = flattenGraph(msg.graph, msg.url)
	}
	m.graphIdx = 0

	if m.ready {
		m.viewport.SetContent(m.renderCurrentGraphSubView())
		m.viewport.GotoTop()
	}
	return m, nil
}

func (m model) handleFetchResult(msg fetchResult) (tea.Model, tea.Cmd) {
	// Ignore stale results from superseded fetches.
	if msg.seq != m.fetchSeq {
		return m, nil
	}
	m.loading = false
	if msg.err != nil {
		m.err = msg.err
		m.pendingBody = ""
		m.status = ""
		m.metadata = nil
		m.fromCache = false
		m.links = nil
		m.linkIdx = -1
		m.hoverIdx = -1
		m.markedRendered = ""
		m.linkRegions = nil
		m.linkInfos = nil
		if m.ready {
			m.viewport.SetContent(errorView(msg.err))
		}
		return m, nil
	}
	m.err = nil
	m.status = msg.result.Response.Status
	m.metadata = msg.result.Response.Metadata
	m.fromCache = msg.result.FromCache

	// Extract and resolve links from raw body.
	m.rawBody = msg.result.Response.Body
	m.linkInfos = links.ExtractWithPositions(m.rawBody)
	m.links = make([]string, 0, len(m.linkInfos))
	for _, li := range m.linkInfos {
		m.links = append(m.links, links.Resolve(msg.url, li.Dest))
	}
	m.linkIdx = -1
	m.hoverIdx = -1

	// Render markdown with link markers.
	var rendered string
	if m.ready {
		marked := injectLinkMarkers(msg.result.Response.Body, m.linkInfos)
		r, err := m.renderMarkdown(marked)
		if err != nil {
			rendered = msg.result.Response.Body
			m.markedRendered = ""
		} else {
			m.markedRendered = r
			cleaned, regions := processMarkers(r, m.linkIdx, m.hoverIdx)
			m.linkRegions = regions
			rendered = cleaned
		}
		m.viewport.SetContent(rendered)
		m.viewport.GotoTop()
	} else {
		m.pendingBody = msg.result.Response.Body
	}

	m.history, m.histIdx = pushHistory(m.history, m.histIdx, historyEntry{
		url:            msg.url,
		rendered:       rendered,
		markedRendered: m.markedRendered,
		rawBody:        m.rawBody,
		status:         m.status,
		metadata:       m.metadata,
		links:          m.links,
		linkInfos:      m.linkInfos,
	})

	m.focus = focusViewport
	m.addressBar.Blur()
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}

	if m.focus == focusAddressBar {
		switch msg.Type {
		case tea.KeyEnter:
			raw := m.addressBar.Value()
			if raw != "" {
				m.loading = true
				m.fetchSeq++
				m.err = nil
				m.pendingBody = ""
				return m, m.doFetch(raw)
			}
			return m, nil
		case tea.KeyEscape:
			m.focus = focusViewport
			m.addressBar.Blur()
			return m, nil
		case tea.KeyTab:
			return m.toggleFocus(), nil
		}
		var cmd tea.Cmd
		m.addressBar, cmd = m.addressBar.Update(msg)
		return m, cmd
	}

	// Viewport focused.
	return m.handleViewportKey(msg)
}

func (m model) handleViewportKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Delegate to graph key handler when in graph view.
	if m.viewMode == viewGraph {
		return m.handleGraphKey(msg)
	}

	// When help is showing, any key dismisses it.
	if m.showHelp {
		return m.handleHelpDismiss(msg)
	}

	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "esc":
		if m.status == "bookmarks" {
			if m.histIdx >= 0 {
				m.restoreHistory()
			} else {
				m.status = ""
				m.links = nil
				m.rawBody = ""
				m.linkIdx = -1
				m.metadata = nil
				if m.ready {
					m.viewport.SetContent("")
				}
			}
			return m, nil
		}
	case "?":
		m.showHelp = true
		if m.ready {
			m.viewport.SetContent(helpText)
			m.viewport.GotoTop()
		}
		return m, nil
	case "f":
		return m.toggleFocus(), textinput.Blink
	case "g":
		m.viewport.GotoTop()
		return m, nil
	case "G":
		m.viewport.GotoBottom()
		return m, nil
	case "[", "alt+left":
		if m.canGoBack() {
			m.histIdx--
			m.restoreHistory()
		}
		return m, nil
	case "]", "alt+right":
		if m.canGoForward() {
			m.histIdx++
			m.restoreHistory()
		}
		return m, nil
	case "tab":
		return m.handleTabNavigation()
	case "enter":
		return m.handleLinkFollow()
	case "b":
		return m.handleBookmarkToggle()
	case "B":
		return m.handleBookmarkView()
	case "d":
		return m.handleGraphToggle()
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m model) toggleFocus() model {
	if m.focus == focusAddressBar {
		m.focus = focusViewport
		m.addressBar.Blur()
	} else {
		m.focus = focusAddressBar
		m.addressBar.Focus()
	}
	return m
}

func (m model) handleHelpDismiss(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "q" {
		return m, tea.Quit
	}
	m.showHelp = false
	if m.histIdx >= 0 {
		m.restoreHistory()
	} else if m.ready {
		m.viewport.SetContent("")
	}
	return m, nil
}

func (m model) handleTabNavigation() (tea.Model, tea.Cmd) {
	if len(m.links) > 0 {
		m.linkIdx = (m.linkIdx + 1) % (len(m.links) + 1)
		if m.linkIdx == len(m.links) {
			m.linkIdx = -1
		}
		// Re-highlight without re-rendering glamour.
		if m.markedRendered != "" {
			cleaned, regions := processMarkers(m.markedRendered, m.linkIdx, m.hoverIdx)
			m.linkRegions = regions
			m.viewport.SetContent(cleaned)
			// Scroll to keep the selected link visible.
			if m.linkIdx >= 0 {
				for _, r := range regions {
					if r.idx != m.linkIdx {
						continue
					}
					if r.line < m.viewport.YOffset {
						m.viewport.SetYOffset(r.line)
					} else if r.line >= m.viewport.YOffset+m.viewport.Height {
						m.viewport.SetYOffset(r.line - m.viewport.Height + 1)
					}
					break
				}
			}
		}
	}
	return m, nil
}

func (m model) handleLinkFollow() (tea.Model, tea.Cmd) {
	if m.linkIdx >= 0 && m.linkIdx < len(m.links) {
		target := m.links[m.linkIdx]
		m.addressBar.SetValue(target)
		m.loading = true
		m.fetchSeq++
		m.links = nil
		m.linkIdx = -1
		return m, m.doFetch(target)
	}
	return m, nil
}

func (m model) handleGraphToggle() (tea.Model, tea.Cmd) {
	url := m.addressBar.Value()
	if url == "" {
		return m, nil
	}

	m.viewMode = viewGraph
	m.graphSubView = subViewLinks
	m.crawling = true
	m.crawlSeq++
	m.graphIdx = 0

	// Seed from persistent store for instant display while crawl runs.
	if m.graphStore != nil && m.graphStore.NodeCount() > 0 {
		m.graphData = m.graphStore.ToGraph()
		m.graphNodes = flattenGraph(m.graphData, url)
	} else {
		m.graphNodes = nil
		m.graphData = nil
	}

	if m.ready {
		if len(m.graphNodes) > 0 {
			m.viewport.SetContent(renderGraphView(m.graphNodes, m.graphIdx, m.width))
		} else {
			m.viewport.SetContent("\n  Crawling document links...")
		}
		m.viewport.GotoTop()
	}
	return m, m.startCrawl(url)
}

func (m model) handleBookmarkToggle() (tea.Model, tea.Cmd) {
	url := m.addressBar.Value()
	if url == "" || m.bookmarkStore == nil {
		return m, nil
	}
	if m.bookmarkStore.Has(url) {
		if err := m.bookmarkStore.Remove(url); err != nil {
			m.bookmarkMsg = "Failed to remove bookmark: " + err.Error()
		} else {
			m.bookmarkMsg = "Bookmark removed"
		}
	} else {
		title := links.ExtractTitle(m.rawBody)
		if title == "" {
			title = url
		}
		if err := m.bookmarkStore.Add(url, title); err != nil {
			m.bookmarkMsg = "Failed to bookmark: " + err.Error()
		} else {
			m.bookmarkMsg = "Bookmarked!"
		}
	}
	m.bookmarkSeq++
	seq := m.bookmarkSeq
	return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		return clearBookmarkMsg{seq: seq}
	})
}

func (m model) handleBookmarkView() (tea.Model, tea.Cmd) {
	if m.bookmarkStore == nil {
		return m, nil
	}
	body := m.bookmarkStore.Render()
	m.rawBody = body
	raw := links.Extract(body)
	m.links = make([]string, 0, len(raw))
	for _, dest := range raw {
		m.links = append(m.links, links.Resolve("", dest))
	}
	m.linkIdx = -1
	m.status = "bookmarks"
	m.addressBar.SetValue("")
	m.loading = false
	m.fetchSeq++
	m.metadata = nil
	m.fromCache = false
	m.err = nil
	if m.ready {
		rendered, err := m.renderMarkdown(body)
		if err != nil {
			m.viewport.SetContent(body)
		} else {
			m.viewport.SetContent(rendered)
		}
		m.viewport.GotoTop()
	}
	return m, nil
}

func (m model) View() string {
	if !m.ready {
		return "Loading..."
	}

	var b strings.Builder

	// Address bar.
	barStyle := lipgloss.NewStyle().
		Padding(0, 1).
		Width(m.width)
	if m.focus == focusAddressBar {
		barStyle = barStyle.Bold(true)
	}
	b.WriteString(barStyle.Render(m.addressBar.View()))
	b.WriteByte('\n')

	// Divider.
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteByte('\n')

	// Viewport.
	b.WriteString(m.viewport.View())
	b.WriteByte('\n')

	// Status bar.
	b.WriteString(m.statusBarView())

	return b.String()
}

func (m model) statusBarView() string {
	style := lipgloss.NewStyle().
		Width(m.width).
		Padding(0, 1)

	if m.viewMode == viewGraph {
		if m.crawling {
			return style.Render("Crawling...")
		}
		var viewName string
		switch m.graphSubView {
		case subViewBacklinks:
			viewName = "Backlinks"
		case subViewTopology:
			viewName = "Topology"
		default:
			viewName = "Links"
		}
		if m.graphData != nil || m.graphSubView != subViewLinks {
			var hint string
			if m.graphData != nil {
				hint = fmt.Sprintf("%s  |  %d nodes, %d edges  |  d/r/t views  |  ↑↓ select  |  Enter navigate",
					viewName, m.graphData.NodeCount(), m.graphData.EdgeCount())
			} else {
				hint = fmt.Sprintf("%s  |  d/r/t views  |  ↑↓ select  |  Enter navigate", viewName)
			}
			return style.Foreground(lipgloss.Color("14")).Render(hint)
		}
		return style.Render("")
	}
	if m.showHelp {
		return style.Faint(true).Render("Press any key to dismiss")
	}
	if m.loading {
		return style.Render("Loading...")
	}
	if m.err != nil {
		return style.Foreground(lipgloss.Color("9")).Render("Error: " + m.err.Error())
	}

	// Show transient bookmark message.
	if m.bookmarkMsg != "" {
		return style.Foreground(lipgloss.Color("10")).Render(m.bookmarkMsg)
	}

	// Show selected link in status bar (link navigation mode).
	if m.linkIdx >= 0 && m.linkIdx < len(m.links) {
		hint := fmt.Sprintf("[%d/%d] %s", m.linkIdx+1, len(m.links), m.links[m.linkIdx])
		return style.Foreground(lipgloss.Color("12")).Render(hint)
	}

	if m.status == "" {
		return style.Faint(true).Render("Enter a mark:// URL and press Enter  |  ? for help")
	}

	parts := []string{"[" + m.status + "]"}
	if m.status != "bookmarks" && m.bookmarkStore != nil && m.bookmarkStore.Has(m.addressBar.Value()) {
		parts = append(parts, "★")
	}
	if m.fromCache {
		parts = append(parts, "(cached)")
	}
	if v, ok := m.metadata["version"]; ok {
		parts = append(parts, "v"+v)
	}
	if mod, ok := m.metadata["modified"]; ok {
		parts = append(parts, mod)
	}
	scroll := fmt.Sprintf("%d%%", int(m.viewport.ScrollPercent()*100))
	parts = append(parts, scroll)

	if m.status != protocol.StatusOK && m.status != "bookmarks" {
		style = style.Foreground(lipgloss.Color("11"))
	}
	return style.Render(strings.Join(parts, "  "))
}

func (m model) doFetch(raw string) tea.Cmd {
	seq := m.fetchSeq
	return func() tea.Msg {
		host, path, err := fetch.ParseMarkURL(raw)
		if err != nil {
			return fetchResult{err: err, url: raw, seq: seq}
		}
		result, err := m.client.Fetch(host, path, tokens.Resolve("", host, tokens.LoadDefault()))
		return fetchResult{result: result, err: err, url: raw, seq: seq}
	}
}

// linkRegion maps a link index to its position in the rendered output.
type linkRegion struct {
	idx      int // index into m.links
	line     int // 0-based line number in rendered output
	startCol int // start column (visual, excluding ANSI codes)
	endCol   int // end column (visual, excluding ANSI codes)
}

const maxMarkedLinks = 0x1000 // 4096 links max to avoid marker range overlap

// markerStart returns the start marker rune for link index i.
func markerStart(i int) rune { return rune(0xF0000 + i) }

// markerEnd returns the end marker rune for link index i.
func markerEnd(i int) rune { return rune(0xF1000 + i) }

// injectLinkMarkers inserts unique Unicode markers around each link's text in the
// raw markdown source. Markers are placed inside the brackets: [⟪text⟫](url).
// Builds a sorted list of insertions and applies them in a single forward pass.
func injectLinkMarkers(body string, infos []links.LinkInfo) string {
	if len(infos) == 0 {
		return body
	}
	if len(infos) > maxMarkedLinks {
		infos = infos[:maxMarkedLinks]
	}

	// Build insertion points sorted by byte offset.
	// Each link needs two insertions: start marker after '[', end marker before ']'.
	type insertion struct {
		pos    int
		marker string
	}
	insertions := make([]insertion, 0, len(infos)*2)
	for i, info := range infos {
		if info.OpenBracket < 0 {
			continue // no text nodes — skip marker injection
		}
		insertions = append(insertions,
			insertion{pos: info.OpenBracket + 1, marker: string(markerStart(i))},
			insertion{pos: info.CloseBracket, marker: string(markerEnd(i))},
		)
	}
	// Sort by position (links are already in document order, but start/end interleave).
	sort.Slice(insertions, func(a, b int) bool {
		return insertions[a].pos < insertions[b].pos
	})

	// Single-pass build.
	var result strings.Builder
	result.Grow(len(body) + len(infos)*8)
	prev := 0
	for _, ins := range insertions {
		result.WriteString(body[prev:ins.pos])
		result.WriteString(ins.marker)
		prev = ins.pos
	}
	result.WriteString(body[prev:])
	return result.String()
}

// isHighlighted reports whether a link index should be visually highlighted.
// Hover takes priority, but Tab selection is also shown.
func isHighlighted(idx, selectedIdx, hoverIdx int) bool {
	return idx == hoverIdx || idx == selectedIdx
}

// processMarkers scans rendered ANSI output for link markers, records their
// positions as linkRegions (including the URL glamour renders after the text),
// and highlights links matching selectedIdx or hoverIdx with reverse video.
// Marker state is tracked across the entire string to handle line-wrapped links.
func processMarkers(rendered string, selectedIdx, hoverIdx int) (string, []linkRegion) {
	var regions []linkRegion
	var result strings.Builder
	result.Grow(len(rendered))

	lineNum := 0
	visualCol := 0
	inEscape := false

	openIdx := -1
	openCol := 0

	extendingIdx := -1
	extendingCol := 0
	seenURLChar := false

	runes := []rune(rendered)
	for i, r := range runes {
		// ANSI escape sequence: \x1b[...m
		if r == '\x1b' && i+1 < len(runes) && runes[i+1] == '[' {
			inEscape = true
			result.WriteRune(r)
			continue
		}
		if inEscape {
			result.WriteRune(r)
			if r == 'm' {
				inEscape = false
			}
			continue
		}

		// Newline resets visual column and may terminate URL extension.
		if r == '\n' {
			if extendingIdx >= 0 {
				finishExtend(&regions, &result, extendingIdx, lineNum, openCol, extendingCol, selectedIdx, hoverIdx)
				extendingIdx = -1
				seenURLChar = false
			}
			result.WriteRune(r)
			lineNum++
			visualCol = 0
			continue
		}

		// If extending past end marker to capture URL, check for termination.
		if extendingIdx >= 0 {
			if r == ' ' && seenURLChar {
				finishExtend(&regions, &result, extendingIdx, lineNum, openCol, extendingCol, selectedIdx, hoverIdx)
				extendingIdx = -1
				seenURLChar = false
				result.WriteRune(r)
				visualCol++
				continue
			}
			if r == ' ' {
				extendingCol++
				result.WriteRune(r)
				visualCol++
				continue
			}
			seenURLChar = true
			extendingCol++
			result.WriteRune(r)
			visualCol++
			continue
		}

		// Check for start marker (U+F0000 + idx).
		if r >= 0xF0000 && r < 0xF1000 {
			idx := int(r - 0xF0000)
			openIdx = idx
			openCol = visualCol
			if isHighlighted(idx, selectedIdx, hoverIdx) {
				result.WriteString("\x1b[7m") // reverse video on
			}
			continue
		}

		// Check for end marker (U+F1000 + idx).
		if r >= 0xF1000 && r < 0xF2000 {
			idx := int(r - 0xF1000)
			if openIdx == idx {
				extendingIdx = idx
				extendingCol = visualCol
				openIdx = -1
			}
			continue
		}

		result.WriteRune(r)
		visualCol++
	}

	// Flush state at end of input.
	if extendingIdx >= 0 {
		finishExtend(&regions, &result, extendingIdx, lineNum, openCol, extendingCol, selectedIdx, hoverIdx)
	} else if openIdx >= 0 && isHighlighted(openIdx, selectedIdx, hoverIdx) {
		result.WriteString("\x1b[27m") // close unclosed highlight
	}

	return result.String(), regions
}

// finishExtend closes an extending link region and turns off highlight if needed.
func finishExtend(regions *[]linkRegion, result *strings.Builder, idx, lineNum, startCol, endCol, selectedIdx, hoverIdx int) {
	*regions = append(*regions, linkRegion{
		idx:      idx,
		line:     lineNum,
		startCol: startCol,
		endCol:   endCol,
	})
	if isHighlighted(idx, selectedIdx, hoverIdx) {
		result.WriteString("\x1b[27m") // reverse video off
	}
}

func (m *model) renderMarkdown(body string) (string, error) {
	wrapWidth := m.width - 4
	if m.renderer == nil || m.rendererWidth != wrapWidth {
		r, err := glamour.NewTermRenderer(
			glamour.WithStandardStyle(m.styleName),
			glamour.WithWordWrap(wrapWidth),
		)
		if err != nil {
			return "", err
		}
		m.renderer = r
		m.rendererWidth = wrapWidth
	}
	return m.renderer.Render(body)
}

func errorView(err error) string {
	return fmt.Sprintf("\n  Error: %s\n", err.Error())
}

// detectStyle probes the terminal background and returns "dark" or "light".
// Must be called before Bubbletea starts, since Bubbletea takes over stdin.
func detectStyle() string {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return "dark"
	}
	if termenv.HasDarkBackground() {
		return "dark"
	}
	return "light"
}

func main() {
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	style := flag.String("style", "auto", "color style: dark, light, or auto")
	flag.Parse()

	styleName := *style
	if styleName == "auto" {
		styleName = detectStyle()
	}
	switch styleName {
	case "dark", "light":
	default:
		fmt.Fprintf(os.Stderr, "invalid style %q: must be dark, light, or auto\n", styleName)
		os.Exit(1)
	}

	client := fetch.NewClient(fetch.Options{
		Cache:    cache.New(cache.DefaultDir()),
		Insecure: *insecure,
	})
	defer client.Close()

	initialURL := ""
	if flag.NArg() > 0 {
		initialURL = flag.Arg(0)
	}

	p := tea.NewProgram(
		initialModel(initialURL, client, styleName),
		tea.WithAltScreen(),
		tea.WithMouseAllMotion(),
	)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
