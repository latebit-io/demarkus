package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit/demarkus/client/internal/cache"
	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/protocol"
)

type focus int

const (
	focusAddressBar focus = iota
	focusViewport
)

type model struct {
	addressBar textinput.Model
	viewport   viewport.Model
	focus      focus
	status     string
	metadata   map[string]string
	fromCache  bool
	err        error
	loading    bool
	cache      *cache.Cache
	width      int
	height     int
	ready      bool
}

type fetchResult struct {
	result fetch.Result
	err    error
	url    string
}

func initialModel(initialURL string, c *cache.Cache) model {
	ti := textinput.New()
	ti.Placeholder = "mark://host:port/path"
	ti.Prompt = " "
	ti.SetValue(initialURL)
	ti.Focus()

	return model{
		addressBar: ti,
		focus:      focusAddressBar,
		cache:      c,
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
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		headerHeight := 2 // address bar + divider
		footerHeight := 1 // status bar
		viewportHeight := m.height - headerHeight - footerHeight
		if viewportHeight < 1 {
			viewportHeight = 1
		}

		if !m.ready {
			m.viewport = viewport.New(m.width, viewportHeight)
			m.ready = true
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
			m.viewport.SetContent(errorView(msg.err))
			return m, nil
		}
		m.err = nil
		m.status = msg.result.Response.Status
		m.metadata = msg.result.Response.Metadata
		m.fromCache = msg.result.FromCache

		rendered, err := renderMarkdown(msg.result.Response.Body, m.width)
		if err != nil {
			m.viewport.SetContent(msg.result.Response.Body)
		} else {
			m.viewport.SetContent(rendered)
		}
		m.viewport.GotoTop()
		m.focus = focusViewport
		m.addressBar.Blur()
		return m, nil
	}

	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyTab:
		return m.toggleFocus(), nil
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
		}
		var cmd tea.Cmd
		m.addressBar, cmd = m.addressBar.Update(msg)
		return m, cmd
	}

	// Viewport focused.
	switch msg.String() {
	case "q":
		return m, tea.Quit
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

	return style.Render(strings.Join(parts, "  "))
}

func (m model) doFetch(raw string) tea.Cmd {
	return func() tea.Msg {
		host, path, err := parseMarkURL(raw)
		if err != nil {
			return fetchResult{err: err, url: raw}
		}
		result, err := fetch.Fetch(host, path, protocol.VerbFetch, m.cache)
		return fetchResult{result: result, err: err, url: raw}
	}
}

func parseMarkURL(raw string) (host, path string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "mark" {
		return "", "", fmt.Errorf("unsupported scheme: %s (expected mark://)", u.Scheme)
	}
	host = u.Host
	if u.Port() == "" {
		host = fmt.Sprintf("%s:%d", u.Hostname(), protocol.DefaultPort)
	}
	path = u.Path
	if path == "" {
		path = "/"
	}
	return host, path, nil
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

func defaultCacheDir() string {
	if dir := os.Getenv("DEMARKUS_CACHE_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".mark", "cache")
	}
	return filepath.Join(home, ".mark", "cache")
}

func main() {
	c := cache.New(defaultCacheDir())

	initialURL := ""
	if len(os.Args) > 1 {
		initialURL = os.Args[1]
	}

	p := tea.NewProgram(
		initialModel(initialURL, c),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
