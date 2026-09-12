package answerbench

import (
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/lookupexpand"
	"github.com/latebit-io/demarkus/client/mdoutline"
)

func (f *Fixture) observed(trace *Trace, host string) (map[string][]string, error) {
	out := make(map[string][]string)
	for _, call := range trace.Calls {
		if call.Error != "" {
			continue
		}
		switch call.Name {
		case "fixture_mark_fetch":
			observation, ok := fetchedObservation(&call, host)
			if ok {
				if err := f.addObservation(out, observation.source, observation.body, observation.source.Anchor == ""); err != nil {
					return nil, err
				}
			}
		case "fixture_mark_lookup":
			if err := f.lookupObservations(out, call.Output, host); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

type fetchObservation struct {
	source Evidence
	body   string
}

func fetchedObservation(call *ToolCall, host string) (fetchObservation, bool) {
	raw, _ := call.Input["url"].(string)
	e, err := location(raw, host)
	if err != nil {
		return fetchObservation{}, false // Invalid tool locations cannot support citations.
	}
	header, body, ok := strings.Cut(call.Output, "\n\n")
	meta := make(map[string]string)
	for line := range strings.SplitSeq(header, "\n") {
		if key, value, found := strings.Cut(line, ": "); found {
			meta[key] = value
		}
	}
	if !ok || meta["status"] != "ok" || meta["mode"] == "outline" {
		return fetchObservation{}, false
	}
	version, err := strconv.Atoi(meta["version"])
	if err != nil || version < 1 || (e.Version != 0 && e.Version != version) {
		return fetchObservation{}, false // Missing or inconsistent revision evidence is unsupported.
	}
	if section, found := meta["section"]; found {
		anchor, valid := strings.CutPrefix(section, "#")
		if !valid || anchor == "" || (e.Anchor != "" && e.Anchor != anchor) {
			return fetchObservation{}, false
		}
		e.Anchor = anchor
	}
	e.Version = version
	return fetchObservation{source: e, body: body}, true
}

func (f *Fixture) lookupObservations(out map[string][]string, text, host string) error {
	var current Evidence
	var body strings.Builder
	flush := func() error {
		if current.Path == "" {
			return nil
		}
		return f.addObservation(out, current, body.String(), false)
	}
	for line := range strings.SplitSeq(text, "\n") {
		raw, frame := strings.CutPrefix(line, lookupexpand.Delimiter+" ")
		if !frame {
			body.WriteString(line + "\n")
			continue
		}
		if err := flush(); err != nil {
			return err
		}
		body.Reset()
		current = Evidence{}
		if strings.HasPrefix(raw, "note: ") {
			continue
		}
		e, err := location(raw, host)
		if err != nil {
			continue // Malformed frames are not source evidence.
		}
		e.Version = f.Latest(e.Path)
		current = e
	}
	return flush()
}

func (f *Fixture) addObservation(out map[string][]string, e Evidence, body string, wholeFetch bool) error {
	section, err := f.Section(e)
	if err != nil || !section.Found {
		return err
	}
	text := normalize(body)
	if e.Anchor != "" || wholeFetch {
		out[sourceKey(e)] = append(out[sourceKey(e)], text)
	}
	// Use global anchors/ranges, not re-slugged partial text: duplicate headings
	// retain their original suffixes, and sibling sections cannot leak into scope.
	for _, h := range mdoutline.Headings(section.body) {
		if h.Start < section.start || h.End > section.end {
			continue
		}
		part := normalize(section.body[h.Start:h.End])
		if strings.Contains(text, part) {
			child := e
			child.Anchor = h.Anchor
			out[sourceKey(child)] = append(out[sourceKey(child)], part)
		}
	}
	return nil
}

func observedQuote(observed map[string][]string, e Evidence, quote string) bool {
	for _, text := range observed[sourceKey(e)] {
		if strings.Contains(text, quote) {
			return true
		}
	}
	e.Anchor = ""
	for _, text := range observed[sourceKey(e)] {
		if strings.Contains(text, quote) {
			return true
		}
	}
	return false
}
