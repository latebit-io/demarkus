package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/internal/bookmarks"
	"github.com/latebit-io/demarkus/client/internal/cache"
	"github.com/latebit-io/demarkus/client/internal/launcher"
	"github.com/latebit-io/demarkus/client/internal/termlinks"
	"github.com/latebit-io/demarkus/client/internal/tokens"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/memory/graph"
	"github.com/latebit-io/demarkus/client/memory/graphstore"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/hiddentext"
	"golang.org/x/term"
)

type focus int

const (
	focusAddressBar focus = iota
	focusViewport
)

// historyEntry is a visited page for back/forward without a fetch. It holds
// the body once; rendering and links are derived again on restore, at the
// width the terminal has then.
type historyEntry struct {
	url      string
	rawBody  string
	status   string
	metadata map[string]string
}

type model struct {
	addressBar textinput.Model
	// authOrigin is the first host the user named; DEMARKUS_AUTH goes nowhere else.
	authOrigin  string
	viewport    viewport.Model
	focus       focus
	status      string
	metadata    map[string]string
	fromCache   bool
	err         error
	loading     bool
	client      *fetch.Client
	fetcher     documentFetcher // client, narrowed so a test can hold a fetch open
	pendingBody string
	width       int
	height      int
	ready       bool

	// Cached markdown renderer (re-created only when width changes).
	renderer      *glamour.TermRenderer
	rendererWidth int

	// History navigation.
	history []historyEntry
	histIdx int

	// Link navigation
	rawBody    string              // raw markdown body of current page
	links      []string            // resolved absolute mark:// URLs
	linkIdx    int                 // -1 = none selected
	linkInfos  []links.LinkInfo    // extended link info with source positions
	rendered   string              // glamour output of rawBody, unhighlighted; "" when none
	renderedAt int                 // the wrap width rendered was made for
	linkSegs   []termlinks.Segment // each link's cells and bytes in rendered, per line
	hoverIdx   int                 // -1 = no hover, index of link under mouse cursor

	// Fetch sequencing: ignore stale results from superseded fetches.
	// currentURL is the document on screen, "" when none is. The address bar is
	// an input and stops naming it as soon as it is edited.
	currentURL string
	fetchSeq   uint64
	// fetchCancel ends the fetch in flight; initFetch is the one Init starts.
	fetchCancel context.CancelFunc
	initFetch   tea.Cmd

	// Graph view
	viewMode        viewMode
	graphSubView    graphSubView
	graphData       *graph.Graph
	graphNodes      []graphListItem
	graphIdx        int
	graphPageCursor string
	graphPageNext   string
	graphPageBack   []string
	graphPageTotal  int
	crawling        bool
	crawlSeq        uint64
	crawlCancel     context.CancelFunc
	graphWarning    string

	showHelp bool

	// Bookmarks
	bookmarkStore *bookmarks.Store
	bookmarkMsg   string // transient status message
	bookmarkSeq   uint64 // sequence counter for stale clear prevention

	// Persistent graph
	graphStore           *graphstore.Store
	graphGeneration      uint64
	graphSavedGeneration uint64

	// Terminal style — "dark" or "light", resolved once before Bubbletea starts.
	styleName string

	// External links: URL schemes (e.g. "http", "https") to open in the system
	// handler when followed. mark:// is always handled internally. Empty slice
	// disables external launching entirely.
	externalSchemes []string
}

type fetchResult struct {
	result   fetch.Result
	err      error
	graphURL string
	url      string
	seq      uint64
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

// maxHistory caps the pages kept for back/forward.
const maxHistory = 50

// pushHistory appends entry after idx, dropping the forward entries and, past
// maxHistory, the oldest. Dropped entries are cleared: a reslice alone would
// keep their bodies reachable in the backing array.
func pushHistory(history []historyEntry, idx int, entry historyEntry) (updated []historyEntry, newIdx int) {
	clear(history[idx+1:])
	history = append(history[:idx+1], entry)
	if over := len(history) - maxHistory; over > 0 {
		history = slices.Delete(history, 0, over)
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

// restoreHistory shows the current history entry again. No network fetch:
// the body is the snapshot's, rendered for the terminal as it is now.
func (m *model) restoreHistory() {
	entry := m.history[m.histIdx]
	m.addressBar.SetValue(entry.url)
	m.loading = false
	m.fromCache = false
	m.showDocument(&entry)
}

// showDocument makes entry the document on screen: its links, its rendering.
func (m *model) showDocument(entry *historyEntry) {
	m.currentURL = entry.url
	m.status = entry.status
	m.metadata = entry.metadata
	m.rawBody = entry.rawBody
	m.linkInfos = links.ExtractWithPositions(entry.rawBody)
	m.links = make([]string, 0, len(m.linkInfos))
	for _, li := range m.linkInfos {
		m.links = append(m.links, links.Resolve(entry.url, li.Dest))
	}
	m.linkIdx = -1
	m.hoverIdx = -1
	m.err = nil
	if !m.ready {
		m.pendingBody = entry.rawBody
		return
	}
	// An earlier page may still wait for the viewport: it must not replace
	// this one when the ready message arrives.
	m.pendingBody = ""
	m.viewport.SetContent(m.renderBody(entry.rawBody))
	m.viewport.GotoTop()
}

// renderBody renders body and locates its links; the result is the content
// with the selected and hovered links lit. A body the renderer refuses is
// shown as written, hidden text revealed, with no link regions.
func (m *model) renderBody(body string) string {
	rendered, err := m.renderMarkdown(body)
	if err != nil {
		m.rendered, m.linkSegs = "", nil
		return hiddentext.Reveal(body)
	}
	shown := make([]termlinks.Link, len(m.linkInfos))
	for i, li := range m.linkInfos {
		// Glamour renders the revealed body (renderMarkdown); match against it.
		shown[i] = termlinks.Link{Text: hiddentext.Reveal(li.Text), Dest: hiddentext.Reveal(li.Dest)}
	}
	m.rendered, m.renderedAt, m.linkSegs = rendered, m.rendererWidth, termlinks.Locate(rendered, shown)
	return m.paint()
}

// paint is the rendered document with the selected and hovered links lit.
// Selection changes repaint; they never render or lay out again.
func (m *model) paint() string {
	return termlinks.Paint(m.rendered, m.linkSegs, func(i int) bool { return i == m.linkIdx || i == m.hoverIdx })
}

// covering reports an error or the bookmarks shown over history[histIdx]:
// neither is a page, so neither has a URL.
func (m *model) covering() bool { return m.currentURL == "" && m.histIdx >= 0 }

// reshow puts the page back after the help or the graph hid it: the render
// in hand when it is still that page at this width, else a fresh one.
func (m *model) reshow() {
	entry := &m.history[m.histIdx]
	if !m.ready || m.rendered == "" || m.currentURL != entry.url || m.rawBody != entry.rawBody || m.renderedAt != m.width-4 {
		m.restoreHistory()
		return
	}
	m.addressBar.SetValue(entry.url)
	m.loading, m.fromCache = false, false
	m.linkIdx, m.hoverIdx, m.err = -1, -1, nil
	m.viewport.SetContent(m.paint())
	m.viewport.GotoTop()
}

// leaveDocument clears the link state of a document no longer on screen.
func (m *model) leaveDocument() {
	m.links, m.linkInfos = nil, nil
	m.rendered, m.linkSegs = "", nil
	m.linkIdx, m.hoverIdx = -1, -1
}

const helpText = `
  Keyboard Shortcuts

  Navigation
    Enter        Follow selected link / fetch URL
    [ / Alt+Left   Go back
    ] / Alt+Right  Go forward
	    Tab          Cycle through links on page
	    d            Document graph view
	    r            Cached relations view
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

	m := model{
		addressBar:      ti,
		focus:           focusAddressBar,
		client:          client,
		fetcher:         client,
		loading:         initialURL != "",
		histIdx:         -1,
		linkIdx:         -1,
		hoverIdx:        -1,
		bookmarkStore:   bs,
		bookmarkMsg:     bmMsg,
		authOrigin:      originHost(initialURL),
		graphStore:      gs,
		styleName:       styleName,
		externalSchemes: externalSchemes,
	}
	if initialURL != "" {
		m.initFetch = m.startFetch(initialURL)
	}
	return m
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink}
	if m.initFetch != nil {
		cmds = append(cmds, m.initFetch)
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
	case relationRefreshResult:
		return m.handleRelationRefreshResult(msg)
	case graphSaveResult:
		if msg.err == nil {
			m.graphSavedGeneration = max(m.graphSavedGeneration, msg.generation)
			return m, nil
		}
		m.bookmarkMsg = "Graph cache update failed: " + msg.err.Error()
		m.graphWarning = msg.err.Error()
		return m.startBookmarkMsgClear()
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
	if m.rendered == "" || m.showHelp || m.err != nil {
		return m
	}
	newHover := -1
	if msg.Y >= 2 {
		contentLine := msg.Y - 2 + m.viewport.YOffset()
		contentCol := msg.X
		for _, r := range m.linkSegs {
			if r.Line == contentLine && contentCol >= r.StartCol && contentCol < r.EndCol {
				newHover = r.Index
				break
			}
		}
	}
	if newHover != m.hoverIdx {
		m.hoverIdx = newHover
		m.viewport.SetContent(m.paint())
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
	prevOffset := m.viewport.YOffset()
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	// A scroll moves another link under the cursor, so the hover is stale:
	// drop it and repaint. Document view only: the other views do not show
	// m.rendered, and a repaint there would paste the document over them.
	if m.viewMode == viewDocument && !m.showHelp && m.err == nil &&
		m.viewport.YOffset() != prevOffset && m.hoverIdx != -1 && m.rendered != "" {
		m.hoverIdx = -1
		m.viewport.SetContent(m.paint())
	}
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
			for _, r := range m.linkSegs {
				if r.Line != contentLine || contentCol < r.StartCol || contentCol >= r.EndCol {
					continue
				}
				// Regions can outlive the links they index while a fetch is in flight.
				if r.Index < 0 || r.Index >= len(m.links) {
					break
				}
				return m.followLink(m.links[r.Index])
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
		m.rewrapContent()
	}
	m.addressBar.SetWidth(m.width - 2)
	return m, nil
}

// rewrapContent re-renders the viewport content at the current width,
// preserving the approximate scroll position and link selection. History
// needs no refresh: an entry renders at the current width when restored.
func (m *model) rewrapContent() {
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
		// Static plain text; dismissing help restores the document at the new width.
	case m.err != nil:
		m.viewport.SetContent(errorView(m.err))
	case m.rawBody != "":
		percent := m.viewport.ScrollPercent()
		m.viewport.SetContent(m.renderBody(m.rawBody))
		// Use the viewport's own line count so the restored offset matches
		// its internal maxYOffset math exactly. strings.Count(cleaned, "\n")
		// is off-by-one for non-empty buffers without a trailing newline.
		if maxOffset := m.viewport.TotalLineCount() - m.viewport.Height(); maxOffset > 0 {
			m.viewport.SetYOffset(int(percent * float64(maxOffset)))
		} else {
			m.viewport.SetYOffset(0)
		}
	}
}

func (m model) handleViewportReady() (tea.Model, tea.Cmd) {
	if m.pendingBody != "" {
		m.viewport.SetContent(m.renderBody(m.pendingBody))
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
	if m.crawlCancel != nil {
		m.crawlCancel()
		m.crawlCancel = nil
	}
	if msg.err != nil && msg.graph == nil {
		m.viewMode = viewDocument
		m.err = msg.err
		if m.ready {
			m.viewport.SetContent(errorView(msg.err))
		}
		return m, nil
	}
	m.graphData = msg.graph
	m.graphWarning = ""
	if warning := graph.CrawlWarning(msg.err, msg.graph.Outcome); warning != nil {
		m.graphWarning = warning.Error()
	}

	// Recompute display list for the active sub-view.
	switch m.graphSubView {
	case subViewBacklinks:
		if queryErr := m.loadRelationsPage(msg.url, m.graphPageCursor); queryErr != nil {
			m.graphWarning = "query relations: " + queryErr.Error()
		}
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

func (m model) handleRelationRefreshResult(msg relationRefreshResult) (tea.Model, tea.Cmd) {
	if msg.seq != m.crawlSeq || m.graphSubView != subViewBacklinks {
		return m, nil
	}
	m.crawlCancel = nil
	if summary := strings.TrimSpace(msg.summary); summary != "" {
		m.graphWarning = summary
	}
	if err := m.loadRelationsPage(msg.url, m.graphPageCursor); err != nil {
		m.graphWarning = "query relations: " + err.Error()
	}
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
	m.releaseFetch()
	m.loading = false
	if msg.err != nil {
		m.currentURL = ""
		m.err = msg.err
		m.pendingBody = ""
		m.status = ""
		m.metadata = nil
		m.fromCache = false
		m.leaveDocument()
		if m.ready {
			m.viewport.SetContent(errorView(msg.err))
		}
		return m, nil
	}
	m.fromCache = msg.result.FromCache
	var graphSave tea.Cmd
	if m.graphStore != nil && m.graphStore.ObserveDocument(msg.graphURL, graph.FetchResult{
		Status: msg.result.Response.Status, Body: msg.result.Response.Body, Metadata: msg.result.Response.Metadata,
	}) {
		m.graphGeneration++
		graphSave = saveGraphStore(m.graphStore, m.graphGeneration)
	}

	entry := historyEntry{url: msg.url, rawBody: msg.result.Response.Body, status: msg.result.Response.Status, metadata: msg.result.Response.Metadata}
	m.showDocument(&entry)
	m.history, m.histIdx = pushHistory(m.history, m.histIdx, entry)

	m.focus = focusViewport
	m.addressBar.Blur()
	return m, graphSave
}

func (m *model) flushGraphStore() {
	if m.graphStore == nil || m.graphSavedGeneration >= m.graphGeneration {
		return
	}
	if err := m.graphStore.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "graph cache save failed during shutdown: %v\n", err)
		return
	}
	m.graphSavedGeneration = m.graphGeneration
}

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Code == 'c' && msg.Mod == tea.ModCtrl {
		m.cancelCrawl()
		m.flushGraphStore()
		return m, tea.Quit
	}

	if m.focus == focusAddressBar {
		switch msg.Code {
		case tea.KeyEnter:
			raw := m.addressBar.Value()
			if raw != "" {
				m.cancelCrawl()
				m.loading = true
				m.err = nil
				m.pendingBody = ""
				if m.authOrigin == "" {
					m.authOrigin = originHost(raw)
				}
				cmd := m.startFetch(raw) // before the return: it changes m
				return m, cmd
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
		m.cancelCrawl()
		m.flushGraphStore()
		return m, tea.Quit
	case "esc":
		if m.leaveOverlay() {
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
		m.goBack()
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
	case "r":
		return m.handleRelationsToggle()
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

// leaveOverlay leaves the bookmarks or a failed fetch for the page they
// covered; with no page under them the bookmarks just clear. It reports
// whether there was anything to leave.
func (m *model) leaveOverlay() bool {
	switch {
	case m.covering():
		m.restoreHistory()
	case m.status == "bookmarks":
		m.status = ""
		m.rawBody = ""
		m.metadata = nil
		m.leaveDocument()
		if m.ready {
			m.viewport.SetContent("")
		}
	default:
		return false
	}
	return true
}

// goBack shows the previous page. From an error or the bookmarks that is
// the page they covered, not the one before it.
func (m *model) goBack() {
	switch {
	case m.covering():
		m.restoreHistory()
	case m.canGoBack():
		m.histIdx--
		m.restoreHistory()
	}
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
		m.cancelCrawl()
		m.flushGraphStore()
		return m, tea.Quit
	}
	m.showHelp = false
	if m.histIdx >= 0 {
		m.reshow()
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
	// A wrapped link is visible when any of its lines is.
	for _, r := range m.linkSegs {
		if r.Index == idx && r.Line >= top && r.Line < bottom {
			return true
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
	for _, r := range m.linkSegs {
		if r.Line >= top && r.Line < bottom {
			return r.Index
		}
	}
	return -1
}

func (m model) handleTabNavigation() (tea.Model, tea.Cmd) {
	if len(m.links) > 0 {
		// If the current selection is outside the viewport (typically because
		// the user wheel-scrolled away), resume tabbing from the first link
		// visible in the current scroll window rather than snapping back.
		if m.rendered != "" && (m.linkIdx < 0 || !m.linkVisible(m.linkIdx)) {
			if first := m.firstVisibleLinkIdx(); first >= 0 {
				m.linkIdx = first - 1
			}
		}
		m.linkIdx = (m.linkIdx + 1) % (len(m.links) + 1)
		if m.linkIdx == len(m.links) {
			m.linkIdx = -1
		}
		// Re-highlight without re-rendering glamour.
		if m.rendered != "" {
			m.viewport.SetContent(m.paint())
			// Scroll to keep the selected link visible.
			if m.linkIdx >= 0 {
				for _, r := range m.linkSegs {
					if r.Index != m.linkIdx {
						continue
					}
					if r.Line < m.viewport.YOffset() {
						m.viewport.SetYOffset(r.Line)
					} else if r.Line >= m.viewport.YOffset()+m.viewport.Height() {
						m.viewport.SetYOffset(r.Line - m.viewport.Height() + 1)
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
	m.leaveDocument()
	cmd := m.startFetch(target) // before the return: it changes m
	return m, cmd
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
func isExternalURL(raw string) bool {
	scheme := launcher.SchemeOf(raw)
	return scheme != "" && scheme != "mark"
}

func (m model) handleGraphToggle() (tea.Model, tea.Cmd) {
	url := m.currentURL
	if url == "" {
		return m, nil
	}

	m.viewMode = viewGraph
	m.graphSubView = subViewLinks
	m.crawling = true
	if m.crawlCancel != nil {
		m.crawlCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.crawlCancel = cancel
	m.graphWarning = ""
	m.graphPageCursor = ""
	m.graphPageNext = ""
	m.graphPageBack = nil
	m.graphPageTotal = 0
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
			m.viewport.SetContent(m.renderCurrentGraphSubView())
		} else {
			m.viewport.SetContent("\n  Crawling document links...")
		}
		m.viewport.GotoTop()
	}
	return m, m.startCrawl(ctx, url)
}

func (m model) handleBookmarkToggle() (tea.Model, tea.Cmd) {
	url := m.currentURL
	if url == "" || m.bookmarkStore == nil {
		return m, nil
	}
	title := links.ExtractTitle(m.rawBody)
	if title == "" {
		title = url
	}
	// The store decides add or remove on the file as it is now.
	switch added, err := m.bookmarkStore.Toggle(url, title); {
	case err != nil:
		m.bookmarkMsg = "Failed to update bookmarks: " + err.Error()
	case added:
		m.bookmarkMsg = "Bookmarked!"
	default:
		m.bookmarkMsg = "Bookmark removed"
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
	m.addressBar.SetValue("")
	m.loading = false
	m.cancelFetch()
	m.fromCache = false
	// The list is a document of its own with no URL, so it covers the
	// history page: Esc or back returns to it.
	m.showDocument(&historyEntry{status: "bookmarks", rawBody: m.bookmarkStore.Render()})
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
		return style.Foreground(lipgloss.Color("9")).Render(hiddentext.Reveal("Error: " + m.err.Error()))
	}

	// Show transient bookmark message.
	if m.bookmarkMsg != "" {
		return style.Foreground(lipgloss.Color("10")).Render(hiddentext.Reveal(m.bookmarkMsg))
	}

	// Show selected link in status bar (link navigation mode).
	if m.linkIdx >= 0 && m.linkIdx < len(m.links) {
		target := m.links[m.linkIdx]
		prefix := ""
		if isExternalURL(target) {
			prefix = "↗ "
		}
		hint := fmt.Sprintf("[%d/%d] %s%s", m.linkIdx+1, len(m.links), prefix, target)
		return style.Foreground(lipgloss.Color("12")).Render(hiddentext.Reveal(hint))
	}

	if m.status == "" {
		return style.Faint(true).Render("Enter a mark:// URL and press Enter  |  ? for help")
	}

	parts := []string{"[" + m.status + "]"}
	if m.status != "bookmarks" && m.bookmarkStore != nil && m.bookmarkStore.Has(m.currentURL) {
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
	// Status and metadata are the server's words: revealed like a body.
	return style.Render(hiddentext.Reveal(strings.Join(parts, "  ")))
}

// originHost returns the dial host of a user-entered URL, or "" when it does not parse.
func originHost(raw string) string {
	host, err := links.DialHost(raw)
	if err != nil {
		return ""
	}
	return host
}

// documentFetcher is the one verb the document view needs.
type documentFetcher interface {
	Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error)
}

// startFetch supersedes the fetch in flight and returns the command for raw.
func (m *model) startFetch(raw string) tea.Cmd {
	m.cancelFetch()
	ctx, cancel := context.WithCancel(context.Background())
	m.fetchCancel = cancel
	return m.doFetch(ctx, raw)
}

// cancelFetch ends the fetch in flight and makes its result read as stale.
func (m *model) cancelFetch() {
	m.releaseFetch()
	m.fetchSeq++
}

// releaseFetch frees the context of a fetch that finished or is unwanted.
func (m *model) releaseFetch() {
	if m.fetchCancel != nil {
		m.fetchCancel()
		m.fetchCancel = nil
	}
}

// doFetch reads raw under ctx and tags the result with the current sequence.
func (m model) doFetch(ctx context.Context, raw string) tea.Cmd {
	seq := m.fetchSeq
	cred := tokens.Credential{Origin: m.authOrigin}
	return func() tea.Msg {
		target, err := links.ParseMark(raw)
		if err != nil {
			return fetchResult{err: err, url: raw, seq: seq}
		}
		host := target.DialHost()
		token := tokens.Resolve(cred, host, tokens.LoadDefault())
		result, err := m.fetcher.Fetch(ctx, fetch.FetchRequest{Host: host, Path: target.Path, Token: token})
		return fetchResult{result: result, err: err, graphURL: target.NodeURL(), url: raw, seq: seq}
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
	// Stored versions may predate the server's hidden-text gate.
	return m.renderer.Render(hiddentext.Reveal(body))
}

// errorView words a failure. Its text can quote a server or a URL, so
// hidden text is revealed before it reaches the terminal.
func errorView(err error) string {
	return hiddentext.Reveal(fmt.Sprintf("\n  Error: %s\n", err.Error()))
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

	externalSchemes, err := launcher.ParseSchemes(*externalLinks)
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
