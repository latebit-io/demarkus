package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/latebit/demarkus/client/internal/bookmarks"
	"github.com/latebit/demarkus/client/internal/cache"
	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/client/internal/graph"
	"github.com/latebit/demarkus/client/internal/graphstore"
	"github.com/latebit/demarkus/client/internal/launcher"
	"github.com/latebit/demarkus/client/internal/links"
	"github.com/latebit/demarkus/client/internal/tokens"
	"github.com/latebit/demarkus/protocol"
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

	// External links: URL schemes (e.g. "http", "https") to open in the system
	// handler when followed. mark:// is always handled internally. Empty slice
	// disables external launching entirely.
	externalSchemes []string
}

type fetchResult struct {
	result fetch.Result
	err    error
	url    string
	seq    uint64
}

// externalOpenResult is sent after an attempt to open a URL in the system handler.
type externalOpenResult struct {
	url string
	err error
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
			r, err := m.renderMarkdown(entry.rawBody)
			if err != nil {
				content = entry.rawBody
			} else {
				m.markedRendered = injectLinkMarkers(r, entry.linkInfos)
				cleaned, regions := processMarkers(m.markedRendered, m.linkIdx, m.hoverIdx)
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

func initialModel(initialURL string, client *fetch.Client, styleName string, externalSchemes []string) model {
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
		addressBar:      ti,
		focus:           focusAddressBar,
		client:          client,
		loading:         initialURL != "",
		histIdx:         -1,
		linkIdx:         -1,
		hoverIdx:        -1,
		bookmarkStore:   bs,
		bookmarkMsg:     bmMsg,
		graphStore:      gs,
		styleName:       styleName,
		externalSchemes: externalSchemes,
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
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)
	case tea.MouseMotionMsg:
		return m.handleMouseMotion(msg)
	case tea.MouseWheelMsg:
		return m.handleMouseWheel(msg)
	case tea.WindowSizeMsg:
		return m.handleWindowSize(msg)
	case crawlResult:
		return m.handleCrawlResult(msg)
	case fetchResult:
		return m.handleFetchResult(msg)
	case externalOpenResult:
		return m.handleExternalOpenResult(msg)
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

func (m model) handleExternalOpenResult(msg externalOpenResult) (tea.Model, tea.Cmd) {
	switch {
	case msg.err == nil:
		m.bookmarkMsg = "Opened in system handler"
	case errors.Is(msg.err, launcher.ErrDisallowedScheme):
		m.bookmarkMsg = "Scheme not allowed: " + msg.url
	case errors.Is(msg.err, launcher.ErrNoHandler):
		m.bookmarkMsg = "No URL handler available on this system"
	default:
		m.bookmarkMsg = "Failed to open: " + msg.err.Error()
	}
	return m.startBookmarkMsgClear()
}

func (m model) handleMouseHover(msg tea.MouseMotionMsg) model {
	if m.markedRendered == "" || m.showHelp || m.err != nil || m.status == "bookmarks" {
		return m
	}
	newHover := -1
	if msg.Y >= 2 {
		contentLine := msg.Y - 2 + m.viewport.YOffset()
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

func (m model) handleMouseMotion(msg tea.MouseMotionMsg) (tea.Model, tea.Cmd) {
	if m.ready && m.viewMode == viewDocument {
		m = m.handleMouseHover(msg)
	}
	if m.ready {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) handleMouseWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	if !m.ready {
		return m, nil
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if msg.Button == tea.MouseLeft {
		if msg.Y == 0 {
			m.focus = focusAddressBar
			m.addressBar.Focus()
			return m, textinput.Blink
		}
		if msg.Y >= 2 && m.ready && m.viewMode == viewDocument {
			// Check if click lands on a link.
			contentLine := msg.Y - 2 + m.viewport.YOffset()
			contentCol := msg.X
			for _, r := range m.linkRegions {
				if r.line != contentLine || contentCol < r.startCol || contentCol >= r.endCol {
					continue
				}
				return m.followLink(m.links[r.idx])
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
		m.viewport = viewport.New(viewport.WithWidth(m.width), viewport.WithHeight(viewportHeight))
		m.ready = true
		// Defer pending content to a separate update cycle so the
		// viewport has a chance to fully initialise before receiving
		// content. Setting content in the same cycle as creation can
		// leave the viewport blank until the next event (e.g. scroll).
		if m.pendingBody != "" || m.err != nil {
			m.addressBar.SetWidth(m.width - 2)
			return m, func() tea.Msg { return viewportReady{} }
		}
	} else {
		m.viewport.SetWidth(m.width)
		m.viewport.SetHeight(viewportHeight)
		m.rewrapContent(viewportHeight)
	}
	m.addressBar.SetWidth(m.width - 2)
	return m, nil
}

// rewrapContent re-renders the viewport content at the current width,
// preserving the approximate scroll position and link selection.
// Called on window resize so markdown re-wraps to the new width.
func (m *model) rewrapContent(viewportHeight int) {
	switch {
	case m.viewMode == viewGraph:
		switch {
		case len(m.graphNodes) > 0:
			m.viewport.SetContent(m.renderCurrentGraphSubView())
		case m.crawling:
			m.viewport.SetContent("\n  Crawling document links...")
		default:
			m.viewport.SetContent("")
		}
	case m.showHelp:
		// Static plain text; no re-render needed.
	case m.err != nil:
		m.viewport.SetContent(errorView(m.err))
	case m.rawBody != "":
		percent := m.viewport.ScrollPercent()
		r, err := m.renderMarkdown(m.rawBody)
		var cleaned string
		if err != nil {
			cleaned = m.rawBody
			m.markedRendered = ""
			m.linkRegions = nil
		} else {
			m.markedRendered = injectLinkMarkers(r, m.linkInfos)
			var regions []linkRegion
			cleaned, regions = processMarkers(m.markedRendered, m.linkIdx, m.hoverIdx)
			m.linkRegions = regions
		}
		m.viewport.SetContent(cleaned)
		// Persist the new-width render into the active history snapshot so
		// back/forward navigation and help dismissal (which call
		// restoreHistory) don't revert to the pre-resize wrap. Skip when
		// viewing bookmarks — that rawBody doesn't belong to the history
		// entry at m.histIdx.
		if m.histIdx >= 0 && m.histIdx < len(m.history) && m.status != "bookmarks" {
			m.history[m.histIdx].rendered = cleaned
			m.history[m.histIdx].markedRendered = m.markedRendered
		}
		totalLines := strings.Count(cleaned, "\n")
		if maxOffset := totalLines - viewportHeight; maxOffset > 0 {
			m.viewport.SetYOffset(int(percent * float64(maxOffset)))
		} else {
			m.viewport.SetYOffset(0)
		}
	}
}

func (m model) handleViewportReady() (tea.Model, tea.Cmd) {
	if m.pendingBody != "" {
		rendered, err := m.renderMarkdown(m.pendingBody)
		if err != nil {
			m.viewport.SetContent(m.pendingBody)
		} else {
			m.markedRendered = injectLinkMarkers(rendered, m.linkInfos)
			cleaned, regions := processMarkers(m.markedRendered, m.linkIdx, m.hoverIdx)
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

	// Render markdown, then inject link markers into the ANSI output.
	var rendered string
	if m.ready {
		r, err := m.renderMarkdown(msg.result.Response.Body)
		if err != nil {
			rendered = msg.result.Response.Body
			m.markedRendered = ""
		} else {
			m.markedRendered = injectLinkMarkers(r, m.linkInfos)
			cleaned, regions := processMarkers(m.markedRendered, m.linkIdx, m.hoverIdx)
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

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Code == 'c' && msg.Mod == tea.ModCtrl {
		return m, tea.Quit
	}

	if m.focus == focusAddressBar {
		switch msg.Code {
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

func (m model) handleViewportKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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

func (m model) handleHelpDismiss(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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

// linkVisible reports whether link idx's rendered region is within the
// current viewport window. Returns false if the link has no region (e.g.
// an empty-text link that was skipped by marker injection).
func (m model) linkVisible(idx int) bool {
	top := m.viewport.YOffset()
	bottom := top + m.viewport.Height()
	for _, r := range m.linkRegions {
		if r.idx == idx {
			return r.line >= top && r.line < bottom
		}
	}
	return false
}

// firstVisibleLinkIdx returns the index (into m.links) of the first link
// whose rendered region falls inside the current viewport window, or -1
// if no link is currently visible.
func (m model) firstVisibleLinkIdx() int {
	top := m.viewport.YOffset()
	bottom := top + m.viewport.Height()
	for _, r := range m.linkRegions {
		if r.line >= top && r.line < bottom {
			return r.idx
		}
	}
	return -1
}

func (m model) handleTabNavigation() (tea.Model, tea.Cmd) {
	if len(m.links) > 0 {
		// If the current selection is outside the viewport (typically because
		// the user wheel-scrolled away), resume tabbing from the first link
		// visible in the current scroll window rather than snapping back.
		if m.markedRendered != "" && (m.linkIdx < 0 || !m.linkVisible(m.linkIdx)) {
			if first := m.firstVisibleLinkIdx(); first >= 0 {
				m.linkIdx = first - 1
			}
		}
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
					if r.line < m.viewport.YOffset() {
						m.viewport.SetYOffset(r.line)
					} else if r.line >= m.viewport.YOffset()+m.viewport.Height() {
						m.viewport.SetYOffset(r.line - m.viewport.Height() + 1)
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
		return m.followLink(m.links[m.linkIdx])
	}
	return m, nil
}

// followLink dispatches the target URL based on its scheme. mark:// URLs
// fetch normally; allowlisted external schemes are handed to the system
// handler without changing the current document.
func (m model) followLink(target string) (tea.Model, tea.Cmd) {
	if isExternalURL(target) {
		return m.openExternal(target)
	}
	m.addressBar.SetValue(target)
	m.loading = true
	m.fetchSeq++
	m.links = nil
	m.linkIdx = -1
	m.hoverIdx = -1
	return m, m.doFetch(target)
}

// openExternal launches target in the user's system handler if its scheme
// is in the allowlist. Current page state is preserved — the TUI stays on
// the document that contained the link.
func (m model) openExternal(target string) (tea.Model, tea.Cmd) {
	if len(m.externalSchemes) == 0 {
		m.bookmarkMsg = "External links disabled"
		return m.startBookmarkMsgClear()
	}
	return m, func() tea.Msg {
		err := launcher.Open(target, m.externalSchemes)
		return externalOpenResult{url: target, err: err}
	}
}

// startBookmarkMsgClear schedules the transient status message to be cleared.
// Reuses the bookmarkMsg/bookmarkSeq machinery since it already implements
// exactly the "show for 2s, clear on sequence match" pattern we need.
func (m model) startBookmarkMsgClear() (tea.Model, tea.Cmd) {
	m.bookmarkSeq++
	seq := m.bookmarkSeq
	return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		return clearBookmarkMsg{seq: seq}
	})
}

// isExternalURL reports whether raw has a non-empty, non-mark scheme.
// Bare paths, relative references, and mark:// URLs are treated as internal.
// Both authority-form (scheme://...) and opaque-form (mailto:user@...) are
// recognised.
func isExternalURL(raw string) bool {
	scheme := schemeOf(raw)
	return scheme != "" && scheme != "mark"
}

// schemeOf returns the scheme component of raw in canonical lowercase form,
// or the empty string if raw has no scheme. Matches RFC 3986 scheme syntax:
// ALPHA *( ALPHA / DIGIT / + / - / . ) terminated by a colon, before any /, ?,
// #, or end of string. Schemes are case-insensitive per RFC 3986 §3.1; we
// normalise to lowercase here so callers can compare directly.
func schemeOf(raw string) string {
	for i, r := range raw {
		switch {
		case i == 0:
			if !isAlpha(r) {
				return ""
			}
		case r == ':':
			return strings.ToLower(raw[:i])
		case isAlpha(r) || isDigit(r) || r == '+' || r == '-' || r == '.':
			// valid scheme char, continue
		default:
			return ""
		}
	}
	return ""
}

func isAlpha(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }
func isDigit(r rune) bool { return r >= '0' && r <= '9' }

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
	m.linkInfos = links.ExtractWithPositions(body)
	m.links = make([]string, 0, len(m.linkInfos))
	for _, li := range m.linkInfos {
		m.links = append(m.links, links.Resolve("", li.Dest))
	}
	m.linkIdx = -1
	m.hoverIdx = -1
	m.markedRendered = ""
	m.linkRegions = nil
	m.status = "bookmarks"
	m.addressBar.SetValue("")
	m.loading = false
	m.fetchSeq++
	m.metadata = nil
	m.fromCache = false
	m.err = nil
	if m.ready {
		r, err := m.renderMarkdown(body)
		if err != nil {
			m.viewport.SetContent(body)
		} else {
			m.markedRendered = injectLinkMarkers(r, m.linkInfos)
			cleaned, regions := processMarkers(m.markedRendered, m.linkIdx, m.hoverIdx)
			m.linkRegions = regions
			m.viewport.SetContent(cleaned)
		}
		m.viewport.GotoTop()
	}
	return m, nil
}

func (m model) View() tea.View {
	if !m.ready {
		v := tea.NewView("Loading...")
		v.AltScreen = true
		v.MouseMode = tea.MouseModeAllMotion
		return v
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

	v := tea.NewView(b.String())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeAllMotion
	return v
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
		target := m.links[m.linkIdx]
		prefix := ""
		if isExternalURL(target) {
			prefix = "↗ "
		}
		hint := fmt.Sprintf("[%d/%d] %s%s", m.linkIdx+1, len(m.links), prefix, target)
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

// injectLinkMarkers inserts unique Unicode markers around each link's rendered text
// in Glamour's ANSI output. Markers are placed by matching LinkInfo.Text against
// the visible (non-ANSI) content. This runs AFTER Glamour rendering so markers
// never affect word wrapping width calculations.
func injectLinkMarkers(rendered string, infos []links.LinkInfo) string {
	if len(infos) == 0 {
		return rendered
	}
	if len(infos) > maxMarkedLinks {
		infos = infos[:maxMarkedLinks]
	}

	runes := []rune(rendered)
	hyperlinks := findHyperlinkRegions(runes)

	type insertion struct {
		runePos int
		marker  string
	}
	var insertions []insertion

	// Match each link to an OSC 8 hyperlink region by URL and visible text.
	used := make([]bool, len(hyperlinks))
	for i, info := range infos {
		if info.Text == "" {
			continue
		}
		for j, hl := range hyperlinks {
			if used[j] {
				continue
			}
			hlURL := hl.url
			if idx := strings.IndexByte(hlURL, '#'); idx != -1 {
				hlURL = hlURL[:idx]
			}
			urlMatch := hlURL == info.Dest || strings.HasSuffix(hlURL, info.Dest) || strings.HasSuffix(info.Dest, hlURL)
			if urlMatch && hl.text == info.Text {
				insertions = append(insertions,
					insertion{runePos: hl.textStart, marker: string(markerStart(i))},
					insertion{runePos: hl.textEnd, marker: string(markerEnd(i))},
				)
				used[j] = true
				break
			}
		}
	}
	if len(insertions) == 0 {
		return rendered
	}

	// Sort insertions by position (URL matching may find links out of order).
	sort.Slice(insertions, func(a, b int) bool {
		return insertions[a].runePos < insertions[b].runePos
	})

	// Build result with marker insertions.
	var result strings.Builder
	result.Grow(len(rendered) + len(insertions)*4)
	prev := 0
	for _, ins := range insertions {
		for _, r := range runes[prev:ins.runePos] {
			result.WriteRune(r)
		}
		result.WriteString(ins.marker)
		prev = ins.runePos
	}
	for _, r := range runes[prev:] {
		result.WriteRune(r)
	}
	return result.String()
}

// hyperlinkRegion describes an OSC 8 hyperlink in Glamour's rendered output.
type hyperlinkRegion struct {
	url       string
	text      string // visible text between the hyperlink start and reset
	textStart int    // rune index of the first visible char of the link text
	textEnd   int    // rune index after the last visible char
}

// parseOSC extracts the content of an OSC sequence starting at runes[start]
// (the character after \x1b]). Returns the content string and the rune index
// to resume scanning from (past the terminator).
func parseOSC(runes []rune, start int) (content string, resume int) {
	end := start
	for end < len(runes) {
		if runes[end] == '\x07' {
			break
		}
		if runes[end] == '\x1b' && end+1 < len(runes) && runes[end+1] == '\\' {
			break
		}
		end++
	}
	content = string(runes[start:end])
	if end < len(runes) && runes[end] == '\x07' {
		return content, end
	}
	if end+1 < len(runes) {
		return content, end + 1
	}
	return content, end
}

// textStartAfterOSC returns the rune index where visible text begins after an
// OSC terminator. resume is the value returned by parseOSC: the BEL index or
// the trailing '\' of an ST sequence.
func textStartAfterOSC(runes []rune, resume int) int {
	if resume < len(runes) && runes[resume] == '\x07' {
		return resume + 1
	}
	return resume + 1 // parseOSC already advanced past \x1b
}

// findHyperlinkRegions scans rendered ANSI output for OSC 8 hyperlink regions.
// Each region contains the URL, visible text, and rune positions.
func findHyperlinkRegions(runes []rune) []hyperlinkRegion {
	var regions []hyperlinkRegion
	var currentURL string
	var visibleText strings.Builder
	textStart := 0
	inCSI := false
	inLink := false

	for i := 0; i < len(runes); i++ {
		r := runes[i]

		if r == '\x1b' && i+1 < len(runes) && runes[i+1] == ']' {
			oscContent, resume := parseOSC(runes, i+2)
			escPos := i // position of \x1b before the OSC

			if strings.HasPrefix(oscContent, "8;") {
				if _, url, ok := strings.Cut(oscContent[2:], ";"); ok {
					if url == "" && currentURL != "" {
						regions = append(regions, hyperlinkRegion{
							url:       currentURL,
							text:      visibleText.String(),
							textStart: textStart,
							textEnd:   escPos,
						})
						currentURL = ""
						inLink = false
						visibleText.Reset()
					} else if url != "" {
						currentURL = url
						inLink = true
						visibleText.Reset()
						textStart = textStartAfterOSC(runes, resume)
					}
				}
			}
			i = resume
			continue
		}

		if r == '\x1b' && i+1 < len(runes) && runes[i+1] == '[' {
			inCSI = true
			continue
		}
		if inCSI {
			if r == 'm' {
				inCSI = false
			}
			continue
		}

		if inLink {
			visibleText.WriteRune(r)
		}
	}
	return regions
}

// findVisibleText searches for textRunes in runes starting from the given offset,
// skipping ANSI escape sequences (CSI and OSC) during matching. Returns the rune
// indices (start, end) of the match or (-1, -1) if not found.
func findVisibleText(runes, textRunes []rune, from int) (startRune, endRune int) {
	if len(textRunes) == 0 {
		return -1, -1
	}

	// Build a map of visible character positions (skipping ANSI escape sequences).
	type visChar struct {
		runeIdx int
		r       rune
	}
	var visible []visChar
	inCSI := false
	inOSC := false
	for i := from; i < len(runes); i++ {
		r := runes[i]
		if r == '\x1b' && i+1 < len(runes) {
			switch runes[i+1] {
			case '[':
				inCSI = true
				continue
			case ']':
				inOSC = true
				i++ // skip the ']'
				continue
			}
		}
		if inCSI {
			if r == 'm' {
				inCSI = false
			}
			continue
		}
		if inOSC {
			if r == '\x07' {
				inOSC = false
			} else if r == '\\' && i > 0 && runes[i-1] == '\x1b' {
				inOSC = false
			}
			continue
		}
		visible = append(visible, visChar{i, r})
	}

	// Substring search on visible characters.
	for k := range len(visible) - len(textRunes) + 1 {
		match := true
		for j, tr := range textRunes {
			if visible[k+j].r != tr {
				match = false
				break
			}
		}
		if match {
			return visible[k].runeIdx, visible[k+len(textRunes)-1].runeIdx + 1
		}
	}
	return -1, -1
}

// isHighlighted reports whether a link index should be visually highlighted.
// Hover takes priority, but Tab selection is also shown.
func isHighlighted(idx, selectedIdx, hoverIdx int) bool {
	return idx == hoverIdx || idx == selectedIdx
}

// escapeState tracks whether the scanner is inside a CSI or OSC escape sequence.
type escapeState struct {
	inCSI bool
	inOSC bool
}

// handleEscape checks if rune r (at index i in runes) is part of an ANSI escape
// sequence. If so, it writes the rune to result and returns true. Callers should
// skip all further processing for that rune.
func (es *escapeState) handleEscape(r rune, i int, runes []rune, result *strings.Builder) bool {
	if r == '\x1b' && i+1 < len(runes) {
		switch runes[i+1] {
		case ']':
			es.inOSC = true
			result.WriteRune(r)
			return true
		case '[':
			es.inCSI = true
			result.WriteRune(r)
			return true
		}
	}
	if es.inOSC {
		result.WriteRune(r)
		if r == '\x07' {
			es.inOSC = false
		} else if r == '\\' && i > 0 && runes[i-1] == '\x1b' {
			es.inOSC = false
		}
		return true
	}
	if es.inCSI {
		result.WriteRune(r)
		if r == 'm' {
			es.inCSI = false
		}
		return true
	}
	return false
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

	openIdx := -1
	openCol := 0

	extendingIdx := -1
	extendingCol := 0
	seenURLChar := false

	var esc escapeState

	runes := []rune(rendered)
	for i, r := range runes {
		if esc.handleEscape(r, i, runes, &result) {
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
	if lipgloss.HasDarkBackground(os.Stdin, os.Stdout) {
		return "dark"
	}
	return "light"
}

func main() {
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	style := flag.String("style", "auto", "color style: dark, light, or auto")
	externalLinks := flag.String("external-links", strings.Join(launcher.DefaultAllowlist, ","),
		"comma-separated URL schemes to open in the system handler; empty string disables")
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

	externalSchemes, err := parseSchemeList(*externalLinks)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid -external-links value: %v\n", err)
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
		initialModel(initialURL, client, styleName, externalSchemes),
	)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// parseSchemeList splits a comma-separated scheme list, trimming whitespace
// and dropping empty entries. Returns nil for an empty input (disables external
// launching). Invalid entries fail the whole parse — a misconfigured flag must
// not silently swallow unrecognisable schemes, because the user would assume
// the scheme is enabled when it never matches anything.
func parseSchemeList(csv string) ([]string, error) {
	if csv == "" {
		return nil, nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		if !isValidScheme(s) {
			return nil, fmt.Errorf("invalid URL scheme %q (schemes must match ALPHA *( ALPHA / DIGIT / \"+\" / \"-\" / \".\" ))", s)
		}
		out = append(out, s)
	}
	return out, nil
}

// isValidScheme reports whether s is a syntactically valid URL scheme per
// RFC 3986: starts with ALPHA, followed by any of ALPHA / DIGIT / + / - / .
func isValidScheme(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !isAlpha(r) {
				return false
			}
			continue
		}
		if !isAlpha(r) && !isDigit(r) && r != '+' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}
