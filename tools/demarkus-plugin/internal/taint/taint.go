// Package taint names the untrusted content a session read before a write:
// the web, a browser, a shell download, or a foreign mark:// server. Only the
// agent's own tool_use blocks count, so no tool result can forge or clear one.
package taint

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol/hiddentext"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/facts"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/toolname"
)

// MaxSources bounds what a reason or stamp names; the first reads matter.
const MaxSources = 3

// maxSourceLen keeps one source short enough for a metadata value.
const maxSourceLen = 120

var (
	toolUseMarker = []byte(`"tool_use"`)
	downloaders   = [][]string{{"curl"}, {"wget"}, {"aria2c"}, {"gh", "api"}}
	browserRe     = regexp.MustCompile(`(?i)chrome|browser|playwright|puppeteer|selenium`)
	webToolRe     = regexp.MustCompile(`(?i)fetch|scrape|crawl|search`)
)

// Call is the part of one tool_use block the classifier reads.
type Call struct {
	Name  string
	Input struct {
		URL          string `json:"url"`
		Command      string `json:"command"`
		SubagentType string `json:"subagent_type"`
	}
}

// Of names the untrusted read one call is, or "" when it is none. memoryID
// resolves a demarkus tool to its memory store ("" for any other).
func Of(c *Call, memoryID func(tool string) string) string {
	switch {
	case c.Name == "WebFetch":
		return Clean("WebFetch " + publicURL(c.Input.URL))
	case c.Name == "WebSearch":
		return "WebSearch"
	case (c.Name == "Agent" || c.Name == "Task") && c.Input.SubagentType == "web-fetch":
		return "web-fetch agent"
	case c.Name == "Bash":
		// The program only: its arguments carry URLs and tokens.
		// No run is exempt beside a script Runs could not read: it may be
		// the read, and the run's label is then the only one.
		runs, opaque := facts.Runs(c.Input.Command, downloaders...)
		exempt := len(runs) > 0 && !opaque && !reroutes(c.Input.Command)
		for _, r := range runs {
			if !exempt || !localCurl(r) {
				return "shell download: " + r.Program
			}
		}
		return ""
	}
	if pt, ok := toolname.Parse(c.Name); ok {
		return foreignRead(c, pt, memoryID)
	}
	if strings.HasPrefix(c.Name, "mcp__") && (browserRe.MatchString(c.Name) || webToolRe.MatchString(c.Name)) {
		return Clean(c.Name)
	}
	return ""
}

// publicURL keeps a URL's scheme and host: tokens ride in userinfo, paths
// (bot tokens, webhooks), queries and fragments, and taint labels are stored.
func publicURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "a URL"
	}
	return u.Scheme + "://" + u.Host
}

// foreignRead flags a soul tool that is not a write and names a full mark://
// URL: the bound store takes bare paths, so a host is someone else's server.
func foreignRead(c *Call, pt toolname.Parsed, memoryID func(string) string) string {
	if pt.StoreWrite() || !strings.HasPrefix(c.Input.URL, "mark://") || memoryID(c.Name) == "" {
		return ""
	}
	target, err := links.ParseMark(c.Input.URL)
	if err != nil {
		return Clean(c.Input.URL)
	}
	return Clean(target.AuthorityURL())
}

// Clean makes a source one short visible line, fit for a metadata value.
func Clean(s string) string {
	s = hiddentext.Reveal(strings.Join(strings.Fields(s), " "))
	if len(s) <= maxSourceLen {
		return s
	}
	n := maxSourceLen
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Merge cleans, dedups and caps sources, first seen first.
func Merge(lists ...[]string) []string {
	var out []string
	for _, list := range lists {
		for _, s := range list {
			if s = Clean(s); s != "" && !slices.Contains(out, s) && len(out) < MaxSources {
				out = append(out, s)
			}
		}
	}
	return out
}

// Transcript scans a Claude Code JSONL transcript and its subagents'
// (<dir>/<session>/subagents/*.jsonl) for untrusted reads, first seen first,
// stopping at MaxSources.
func Transcript(path string) ([]string, error) {
	s := &scanner{ids: map[string]string{}}
	if err := s.file(path); err != nil {
		return nil, err
	}
	subs, err := filepath.Glob(filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	for _, sub := range subs {
		if s.full() {
			break
		}
		if err := s.file(sub); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return s.found, nil
}

// scanner holds one Transcript call's sources and memoized store lookups
// (each toolname.MemoryTargetID reads config files).
type scanner struct {
	found []string
	ids   map[string]string
}

func (s *scanner) full() bool { return len(s.found) >= MaxSources }

func (s *scanner) memoryID(tool string) string {
	id, ok := s.ids[tool]
	if !ok {
		id, _ = toolname.MemoryTargetID(tool) //nolint:errcheck // unreadable config: not the soul, so no foreign read
		s.ids[tool] = id
	}
	return id
}

func (s *scanner) file(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	return s.scan(f)
}

type line struct {
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Call
		} `json:"content"`
	} `json:"message"`
}

// scan reads tool_use blocks only: tool results carry the untrusted text
// itself, and any tool_use shape inside one is a JSON string, not a block.
func (s *scanner) scan(r io.Reader) error {
	br := bufio.NewReaderSize(r, 64*1024)
	for !s.full() {
		raw, err := br.ReadBytes('\n')
		if bytes.Contains(raw, toolUseMarker) {
			s.line(raw, err == nil)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// line adds one transcript line's untrusted reads. A field of an unexpected
// type keeps the rest decoded; a complete line that is not JSON counts as an
// unknown read (fail closed); a partial last line is still being written.
func (s *scanner) line(raw []byte, complete bool) {
	var l line
	err := json.Unmarshal(raw, &l)
	var typeErr *json.UnmarshalTypeError
	switch {
	case err == nil, errors.As(err, &typeErr):
	case complete:
		s.add("unparseable transcript line")
		return
	default:
		return
	}
	for i := range l.Message.Content {
		if b := &l.Message.Content[i]; b.Type == "tool_use" {
			s.add(Of(&b.Call, s.memoryID))
		}
	}
}

func (s *scanner) add(source string) {
	if source != "" && !slices.Contains(s.found, source) && !s.full() {
		s.found = append(s.found, source)
	}
}
