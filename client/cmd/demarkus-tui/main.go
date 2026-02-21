package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit/demarkus/client/internal/cache"
	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/protocol"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

type focus int

const (
	focusAddressBar focus = iota
	focusViewport
)

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

	// History navigation
	history    []string
	histIdx    int
	navigating bool

	// Link navigation
	rawBody string   // raw markdown body of current page
	links   []string // resolved absolute mark:// URLs
	linkIdx int      // -1 = none selected
}

type fetchResult struct {
	result fetch.Result
	err    error
	url    string
}

// pushHistory appends url to history after histIdx, truncating forward entries.
// Caps at 50 entries; returns updated (history, histIdx).
func pushHistory(history []string, idx int, url string) ([]string, int) {
	// Truncate forward entries (everything after idx).
	history = history[:idx+1]
	history = append(history, url)
	// Cap at 50: drop oldest.
	if len(history) > 50 {
		history = history[len(history)-50:]
	}
	return history, len(history) - 1
}

// currentURL returns the URL at the current history position, or "" if history is empty.
func (m model) currentURL() string {
	if m.histIdx < 0 || m.histIdx >= len(m.history) {
		return ""
	}
	return m.history[m.histIdx]
}

// canGoBack reports whether backward navigation is possible.
func (m model) canGoBack() bool {
	return m.histIdx > 0
}

// canGoForward reports whether forward navigation is possible.
func (m model) canGoForward() bool {
	return m.histIdx < len(m.history)-1
}

// extractLinks parses body with goldmark and returns all non-fragment link destinations.
func extractLinks(body string) []string {
	src := []byte(body)
	reader := text.NewReader(src)
	doc := goldmark.DefaultParser().Parse(reader)

	var links []string
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		link, ok := n.(*ast.Link)
		if !ok {
			return ast.WalkContinue, nil
		}
		dest := string(link.Destination)
		if dest != "" && !strings.HasPrefix(dest, "#") {
			links = append(links, dest)
		}
		return ast.WalkContinue, nil
	})
	return links
}

// resolveLink resolves a possibly-relative link dest against currentURL.
func resolveLink(currentURL, dest string) string {
	if strings.Contains(dest, "://") {
		return dest // already absolute
	}
	base, err := url.Parse(currentURL)
	if err != nil || currentURL == "" {
		return dest
	}
	ref, err := url.Parse(dest)
	if err != nil {
		return dest
	}
	return base.ResolveReference(ref).String()
}

func initialModel(initialURL string, client *fetch.Client) model {
	ti := textinput.New()
	ti.Placeholder = "mark://host:port/path"
	ti.Prompt = " "
	ti.SetValue(initialURL)
	ti.Focus()

	return model{
		addressBar: ti,
		focus:      focusAddressBar,
		client:     client,
		loading:    initialURL != "",
		histIdx:    -1,
		linkIdx:    -1,
	}
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink}
	if m.addressBar.Value() != "" {
		cmds = append(cmds, m.doFetch(m.addressBar.Value()))
	}
	return tea.Batch(cmds...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		// Handle clicks to switch focus.
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			if msg.Y == 0 {
				m.focus = focusAddressBar
				m.addressBar.Focus()
				return m, textinput.Blink
			}
			if msg.Y >= 2 {
				m.focus = focusViewport
				m.addressBar.Blur()
			}
		}
		// Forward all mouse events to viewport (scroll wheel, etc).
		if m.ready {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		headerHeight := 2 // address bar + divider
		footerHeight := 1 // status bar
		viewportHeight := max(m.height-headerHeight-footerHeight, 1)

		if !m.ready {
			m.viewport = viewport.New(m.width, viewportHeight)
			m.ready = true
			if m.pendingBody != "" {
				rendered, err := renderMarkdown(m.pendingBody, m.width)
				if err != nil {
					m.viewport.SetContent(m.pendingBody)
				} else {
					m.viewport.SetContent(rendered)
				}
				m.pendingBody = ""
			}
			if m.err != nil {
				m.viewport.SetContent(errorView(m.err))
			}
		} else {
			m.viewport.Width = m.width
			m.viewport.Height = viewportHeight
		}
		m.addressBar.Width = m.width - 2
		return m, nil

	case fetchResult:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			m.status = ""
			m.metadata = nil
			m.fromCache = false
			m.links = nil
			m.linkIdx = -1
			if m.ready {
				m.viewport.SetContent(errorView(msg.err))
			}
			return m, nil
		}
		m.err = nil
		m.status = msg.result.Response.Status
		m.metadata = msg.result.Response.Metadata
		m.fromCache = msg.result.FromCache

		// Push to history unless we're navigating (back/forward).
		shouldPush := !m.navigating
		m.navigating = false
		if shouldPush {
			m.history, m.histIdx = pushHistory(m.history, m.histIdx, msg.url)
		}

		// Extract and resolve links from raw body.
		m.rawBody = msg.result.Response.Body
		raw := extractLinks(m.rawBody)
		m.links = make([]string, 0, len(raw))
		for _, dest := range raw {
			m.links = append(m.links, resolveLink(msg.url, dest))
		}
		m.linkIdx = -1

		if m.ready {
			rendered, err := renderMarkdown(msg.result.Response.Body, m.width)
			if err != nil {
				m.viewport.SetContent(msg.result.Response.Body)
			} else {
				m.viewport.SetContent(rendered)
			}
			m.viewport.GotoTop()
		} else {
			m.pendingBody = msg.result.Response.Body
		}
		m.focus = focusViewport
		m.addressBar.Blur()
		return m, tea.ClearScreen
	}

	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit
	}

	if m.focus == focusAddressBar {
		switch msg.Type {
		case tea.KeyEnter:
			raw := m.addressBar.Value()
			if raw != "" {
				m.loading = true
				m.err = nil
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
	switch msg.String() {
	case "q":
		return m, tea.Quit
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
			url := m.currentURL()
			m.addressBar.SetValue(url)
			m.loading = true
			m.navigating = true
			return m, m.doFetch(url)
		}
		return m, nil
	case "]", "alt+right":
		if m.canGoForward() {
			m.histIdx++
			url := m.currentURL()
			m.addressBar.SetValue(url)
			m.loading = true
			m.navigating = true
			return m, m.doFetch(url)
		}
		return m, nil
	case "tab":
		if len(m.links) > 0 {
			m.linkIdx = (m.linkIdx + 1) % (len(m.links) + 1)
			if m.linkIdx == len(m.links) {
				m.linkIdx = -1 // wrap back to no selection
			}
		}
		return m, nil
	case "enter":
		if m.linkIdx >= 0 && m.linkIdx < len(m.links) {
			url := m.links[m.linkIdx]
			m.addressBar.SetValue(url)
			m.loading = true
			m.links = nil
			m.linkIdx = -1
			return m, m.doFetch(url)
		}
		return m, nil
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

	if m.loading {
		return style.Render("Loading...")
	}
	if m.err != nil {
		return style.Foreground(lipgloss.Color("9")).Render("Error: " + m.err.Error())
	}

	// Show selected link in status bar (link navigation mode).
	if m.linkIdx >= 0 && m.linkIdx < len(m.links) {
		hint := fmt.Sprintf("[%d/%d] %s", m.linkIdx+1, len(m.links), m.links[m.linkIdx])
		return style.Foreground(lipgloss.Color("12")).Render(hint)
	}

	if m.status == "" {
		return style.Faint(true).Render("Enter a mark:// URL and press Enter")
	}

	parts := []string{"[" + m.status + "]"}
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

	if m.status != protocol.StatusOK {
		style = style.Foreground(lipgloss.Color("11"))
	}
	return style.Render(strings.Join(parts, "  "))
}

func (m model) doFetch(raw string) tea.Cmd {
	return func() tea.Msg {
		host, path, err := fetch.ParseMarkURL(raw)
		if err != nil {
			return fetchResult{err: err, url: raw}
		}
		result, err := m.client.Fetch(host, path)
		return fetchResult{result: result, err: err, url: raw}
	}
}

func renderMarkdown(body string, width int) (string, error) {
	r, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(width-4),
	)
	if err != nil {
		return "", err
	}
	return r.Render(body)
}

func errorView(err error) string {
	return fmt.Sprintf("\n  Error: %s\n", err.Error())
}

func main() {
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	flag.Parse()

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
		initialModel(initialURL, client),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
