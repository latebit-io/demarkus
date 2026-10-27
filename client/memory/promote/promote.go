// Package promote is the mechanic behind mark_promote. The soul side stages
// a document and stamps the back-link; the knowledge side checks a draft
// before the surface publishes it. The agent distills and a human gates between.
package promote

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/lookuptable"
	"github.com/latebit-io/demarkus/client/memory/gate"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/layout"
	"github.com/latebit-io/demarkus/protocol/mdoutline"
	"github.com/latebit-io/demarkus/protocol/provenance"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// Phases of one promotion, in order. The soul server serves stage, stamp and
// refresh; the knowledge server dry_run and apply.
const (
	PhaseStage   = "stage"
	PhaseDryRun  = "dry_run"
	PhaseApply   = "apply"
	PhaseStamp   = "stamp"
	PhaseRefresh = "refresh"
)

// Phase sets per surface.
var (
	SoulPhases      = []string{PhaseStage, PhaseStamp, PhaseRefresh}
	KnowledgePhases = []string{PhaseDryRun, PhaseApply}
	AllPhases       = []string{PhaseStage, PhaseDryRun, PhaseApply, PhaseStamp, PhaseRefresh}
)

// Tag marks a promoted source; PromotedFrom names the origin on the copy.
const (
	Tag          = "promoted"
	PromotedFrom = "promoted-from"
)

// Modes of a promoted source: a stub keeps only its title, markers and
// summary; a full one keeps its body.
const (
	ModeStub = "stub"
	ModeFull = "full"
)

// Markers are the back-stamp lines under a promoted document's H1:
// `promoted-via: <server>` and `promoted: <target>@v<N>`.
type Markers struct {
	Via     string
	Target  string
	Version int // the destination version the source tracks; 0 for a legacy marker
}

var markerLine = regexp.MustCompile(`(?m)^promoted(-via)?: (\S.*?)[ \t]*$`)

// Marked reports a back-stamped body: a standalone marker line.
func Marked(body string) bool {
	return strings.Contains(body, "promoted") && markerLine.MatchString(body)
}

// IsPromoted is the one rule every sweep uses: the promoted tag or a marker.
func IsPromoted(meta map[string]string, body string) bool {
	return slices.Contains(protocol.SplitTags(meta["tags"]), Tag) || Marked(body)
}

// ParseMarkers reads the marker lines; found is false without a `promoted:`
// line. A legacy marker without @v<N> leaves Version 0.
func ParseMarkers(body string) (m Markers, found bool) {
	for _, match := range markerLine.FindAllStringSubmatch(body, -1) {
		if match[1] == "-via" {
			m.Via = match[2]
			continue
		}
		found = true
		m.Target, m.Version = splitVersion(match[2])
	}
	return m, found
}

// ParseTarget reads `<target>@v<N>` as a tool argument; both parts are required.
func ParseTarget(s string) (target string, version int, err error) {
	target, version = splitVersion(strings.TrimSpace(s))
	if target == "" || version == 0 {
		return "", 0, fmt.Errorf("target %q: want <url>@v<N>", s)
	}
	return target, version, nil
}

func splitVersion(s string) (target string, version int) {
	t, v, ok := strings.Cut(s, "@v")
	if !ok {
		return s, 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return s, 0
	}
	return t, n
}

// Lines renders the two marker lines.
func (m *Markers) Lines() string {
	return fmt.Sprintf("promoted-via: %s\npromoted: %s@v%d\n", m.Via, m.Target, m.Version)
}

func title(body string) string { return strings.TrimSpace(links.ExtractTitle(body)) }

// h1 is the H1's line span [start, end) in body, or ok false; fenced code
// and setext headings are read as markdown, not as lines starting with #.
func h1(body string) (start, end int, ok bool) {
	for _, h := range mdoutline.Headings(body) {
		if h.Level != 1 {
			continue
		}
		end = h.Start + strings.IndexByte(body[h.Start:], '\n')
		if end < h.Start {
			end = len(body)
		}
		return h.Start, end, true
	}
	return 0, 0, false
}

// WithMarkers puts the markers directly under the H1, replacing any present;
// a body without an H1 opens with them.
func WithMarkers(body string, m *Markers) string {
	body = WithoutMarkers(body)
	_, end, ok := h1(body)
	if !ok {
		return m.Lines() + "\n" + strings.TrimLeft(body, "\n")
	}
	rest := strings.TrimLeft(body[end:], "\n")
	out := body[:end] + "\n\n" + m.Lines()
	if rest == "" {
		return out
	}
	return out + "\n" + rest
}

var blankRun = regexp.MustCompile(`\n{3,}`)

// WithoutMarkers drops the marker lines and the blank run they leave.
func WithoutMarkers(body string) string {
	return blankRun.ReplaceAllString(markerLine.ReplaceAllString(body, ""), "\n\n")
}

// IsStub reports a source reduced to its title, markers and one summary
// paragraph: no further heading, no second paragraph.
func IsStub(body string) bool {
	body = WithoutMarkers(body)
	headings := mdoutline.Headings(body)
	if len(headings) > 1 {
		return false
	}
	if len(headings) == 1 {
		_, end, _ := h1(body)
		body = body[end:]
	}
	return !strings.Contains(strings.TrimSpace(body), "\n\n")
}

func mode(stub bool) string {
	if stub {
		return ModeStub
	}
	return ModeFull
}

// stubBody is a stub's content before its markers: the title, then the summary.
func stubBody(name, summary string) string {
	if summary = strings.TrimSpace(summary); summary == "" {
		return "# " + name + "\n"
	}
	return "# " + name + "\n\n" + summary + "\n"
}

// Stub reduces a body to its title and summary (the metadata description,
// else the opening paragraph).
func Stub(body, description string) string {
	summary := strings.TrimSpace(description)
	if summary == "" {
		summary = mdoutline.OpeningParagraph(WithoutMarkers(body))
	}
	return stubBody(title(body), summary)
}

// metaDigest is the metadata as one deterministic string.
func metaDigest(meta map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(meta)) {
		fmt.Fprintf(&b, "%s=%s\n", k, meta[k])
	}
	return b.String()
}

// Input is the source document as fetched.
type Input struct {
	Path     string
	Body     string
	Version  int
	Metadata map[string]string
}

// Staged is the stage report: the source, its markers and what the layout
// says about it. Packet binds the stamp to this version of the body.
type Staged struct {
	Input
	Markers   Markers
	Promoted  bool
	Stub      bool
	Qualifies string
	Notes     []string
	Packet    string
}

// Stage reads the source for the agent to distill. Nothing is written.
func Stage(in *Input, l *layout.Layout) Staged {
	s := Staged{Input: *in, Qualifies: l.Qualifies(in.Path, in.Metadata), Packet: docwrite.Digest(strconv.Itoa(in.Version), in.Body)}
	s.Markers, s.Promoted = ParseMarkers(in.Body)
	s.Stub = s.Promoted && IsStub(in.Body)
	return s
}

// Report renders the stage answer: the facts, then the body.
func (s *Staged) Report() string {
	var b strings.Builder
	status := "staged"
	if s.Promoted {
		status = "promoted"
	}
	fmt.Fprintf(&b, "status: %s\npath: %s\nversion: %d\nsize: %d\n", status, s.Path, s.Version, len(s.Body))
	if s.Qualifies != "" {
		fmt.Fprintf(&b, "candidate: %s\n", s.Qualifies)
	} else {
		b.WriteString("candidate: no (not a shared type, under the importance floor, or in a staging directory)\n")
	}
	if s.Promoted {
		via := s.Markers.Via
		if via == "" {
			via = "none (legacy marker; the user names the server)"
		}
		fmt.Fprintf(&b, "promoted-via: %s\npromoted: %s@v%d\nmode: %s\n", via, s.Markers.Target, s.Markers.Version, mode(s.Stub))
	}
	for _, n := range s.Notes {
		b.WriteString("note: " + n + "\n")
	}
	fmt.Fprintf(&b, "packet: %s\n", s.Packet)
	for _, k := range []string{"tags", "importance", "type", "description"} {
		if v := s.Metadata[k]; v != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	b.WriteString("\n" + s.Body)
	return b.String()
}

// Refusal is a write the document's state stopped, before anything landed.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

// Written is one soul write that landed: what was sent, plus the response's keys.
type Written struct {
	Path     string
	Version  int
	Mode     string
	Note     string
	Body     string
	Metadata map[string]string
}

// promotedMeta is the metadata a stamped source keeps: carried keys, the
// promoted tag, the surface's identity.
func promotedMeta(meta, identity map[string]string) map[string]string {
	out := docwrite.Carried(meta)
	out["tags"] = strings.Join(protocol.MergeTags(protocol.SplitTags(out["tags"]), []string{Tag}), ",")
	maps.Copy(out, identity)
	return out
}

// Stamp is one back-stamp: the markers to write and whether the body
// becomes a stub. Packet is the stage's; a stub refuses when the document
// changed since, a full stamp adds the markers to the current body.
type Stamp struct {
	Markers Markers
	Stub    bool
	Packet  string
}

// BackStamp writes the markers on the source at its head, one retry on
// conflict for a full stamp.
func BackStamp(ctx context.Context, doc *docwrite.Doc, identity map[string]string, s *Stamp) (Written, error) {
	w := Written{Path: doc.Path, Mode: mode(s.Stub)}
	result, written, err := doc.Edit(ctx, nil, func(head *docwrite.Head) (docwrite.Write, bool, error) {
		if head.Status != protocol.StatusOK {
			return docwrite.Write{}, false, &Refusal{Reason: "fetch " + doc.Path + ": " + head.Status}
		}
		if docwrite.Digest(strconv.Itoa(head.Version), head.Body) != s.Packet {
			if s.Stub {
				return docwrite.Write{}, false, &Refusal{Reason: fmt.Sprintf("the document changed since staging (now v%d); rerun stage", head.Version)}
			}
			w.Note = fmt.Sprintf("the document changed since staging (now v%d); markers added to the current body", head.Version)
		}
		body := head.Body
		if s.Stub {
			body = Stub(body, head.Metadata["description"])
		}
		w.Body, w.Metadata = WithMarkers(body, &s.Markers), promotedMeta(head.Metadata, identity)
		return docwrite.Write{Body: w.Body, Metadata: w.Metadata}, true, nil
	})
	if err != nil {
		return Written{}, err
	}
	if !written && result.Response.Status == protocol.StatusConflict {
		return Written{}, &Refusal{Reason: "conflict: the document moved while stamping; rerun stage"}
	}
	return landed(w, &result, written)
}

// landed words a write's outcome: the version it wrote, or why it did not.
func landed(w Written, result *docwrite.Result, written bool) (Written, error) {
	if !written {
		return Written{}, &Refusal{Reason: "publish " + w.Path + ": " + result.Response.Refusal()}
	}
	version, err := docwrite.Version(result.Response.Metadata)
	if err != nil {
		return Written{}, err
	}
	w.Version = version
	w.Metadata = maps.Clone(w.Metadata)
	maps.Copy(w.Metadata, result.Response.Metadata)
	return w, nil
}

// Refresh statuses.
const (
	RefreshInSync     = "in-sync"
	RefreshStale      = "stale"
	RefreshBehind     = "behind"
	RefreshUnpromoted = "unpromoted"
)

// Refresh is the live knowledge copy as the agent fetched it.
type Refresh struct {
	Live        string
	LiveVersion int
	Target      string // the copy's address; must equal the marker's
}

// Plan is a refresh report: how the source stands against the live copy
// and, when stale, the body and metadata that bring it up to date.
type Plan struct {
	Status   string
	Path     string
	Version  int
	Tracked  int
	Live     int
	Mode     string
	Body     string
	Metadata map[string]string
	Packet   string
}

// PlanRefresh compares the source with the live copy. Stale: a stub gets
// the live opening paragraph as its summary, a full source gets the live
// body; both keep the H1 and take the markers at the live version.
func PlanRefresh(in *Input, r *Refresh) (Plan, error) {
	m, found := ParseMarkers(in.Body)
	p := Plan{Status: RefreshUnpromoted, Path: in.Path, Version: in.Version, Tracked: m.Version, Live: r.LiveVersion, Mode: mode(IsStub(in.Body))}
	if !found {
		return p, nil
	}
	if r.Target != m.Target {
		return Plan{}, fmt.Errorf("target %s is not the marker's %s", r.Target, m.Target)
	}
	switch {
	case r.LiveVersion < m.Version:
		p.Status = RefreshBehind
		return p, nil
	case r.LiveVersion == m.Version:
		p.Status = RefreshInSync
		return p, nil
	}
	p.Status = RefreshStale
	m.Version = r.LiveVersion
	name := title(in.Body)
	if p.Mode == ModeStub {
		p.Body = WithMarkers(stubBody(name, mdoutline.OpeningParagraph(r.Live)), &m)
	} else {
		p.Body = WithMarkers(withTitle(r.Live, name), &m)
	}
	p.Metadata = promotedMeta(in.Metadata, nil)
	p.Packet = docwrite.Digest(strconv.Itoa(in.Version), p.Body, metaDigest(p.Metadata))
	return p, nil
}

// withTitle keeps the source's H1 on the pulled body: the name stays the
// source's, the content becomes the copy's.
func withTitle(live, name string) string {
	start, end, ok := h1(live)
	if !ok {
		return "# " + name + "\n\n" + strings.TrimLeft(live, "\n")
	}
	return live[:start] + "# " + name + live[end:]
}

// Report renders the refresh answer; a stale one ends with the body.
func (p *Plan) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\npath: %s\nversion: %d\n", p.Status, p.Path, p.Version)
	if p.Status == RefreshUnpromoted {
		return b.String()
	}
	fmt.Fprintf(&b, "tracked: v%d\nlive: v%d\nmode: %s\n", p.Tracked, p.Live, p.Mode)
	switch p.Status {
	case RefreshBehind:
		b.WriteString("note: the live version is behind the marker; the destination history is inconsistent, nothing to write\n")
	case RefreshStale:
		fmt.Fprintf(&b, "packet: %s\n\n%s", p.Packet, p.Body)
	}
	return b.String()
}

// ApplyRefresh writes the plan at the version it was planned from; a
// conflict is reported, never retried, so a renewed report precedes the
// next write.
func ApplyRefresh(ctx context.Context, doc *docwrite.Doc, identity map[string]string, p *Plan) (Written, error) {
	if p.Status != RefreshStale {
		return Written{}, &Refusal{Reason: "nothing to write: the document is " + p.Status}
	}
	meta := maps.Clone(p.Metadata)
	maps.Copy(meta, identity)
	result, err := doc.Publish(ctx, docwrite.Write{Body: p.Body, ExpectedVersion: p.Version, Metadata: meta}, merge.OnConflictFail)
	if err != nil {
		return Written{}, err
	}
	if result.Response.Status == protocol.StatusConflict {
		return Written{}, &Refusal{Reason: fmt.Sprintf("the document moved since v%d; rerun refresh", p.Version)}
	}
	return landed(Written{Path: doc.Path, Mode: p.Mode, Body: p.Body, Metadata: meta}, &result, protocol.IsWriteSuccess(result.Response.Status))
}

// Draft is the distilled document the knowledge side checks. Version is
// the target's current version; 0 creates.
type Draft struct {
	Path     string
	Body     string
	Metadata map[string]string
	Source   string // recorded as promoted-from; "" records none
	Version  int
}

// Title is the draft's name: its H1.
func (d *Draft) Title() string { return title(d.Body) }

// SubjectTags are the draft's tags without the axis:value ones, longest
// first: the most specific subjects lead a catalog query.
func (d *Draft) SubjectTags() []string {
	var out []string
	for _, t := range protocol.SplitTags(d.Metadata["tags"]) {
		if !strings.Contains(t, ":") {
			out = append(out, t)
		}
	}
	slices.SortStableFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// Candidate is a catalog row the draft may duplicate. Fetch marks the ones
// whose body the report shows: the target path or a same-title row.
type Candidate struct {
	Path    string
	Title   string
	Tags    string
	Why     string
	Fetch   bool
	Version int
	Body    string
}

// docTitle is the document title of a row: a section row joins the heading
// after a separator.
func docTitle(r lookuptable.Row) string {
	title, _, _ := strings.Cut(r.Title, " › ")
	return title
}

// SimilarCandidates folds the rows of a similar lookup into the candidates:
// a path already listed gains the verdict, a new one is a candidate to read.
func SimilarCandidates(candidates []Candidate, related []string, rows []lookuptable.Row) (merged []Candidate, rest []string) {
	for _, r := range rows {
		why := r.Snippet
		if r.Anchor != "" {
			why += " in #" + r.Anchor
		}
		if i := slices.IndexFunc(candidates, func(c Candidate) bool { return c.Path == r.Path }); i >= 0 {
			candidates[i].Why += ", " + why
			candidates[i].Fetch = true
			continue
		}
		candidates = append(candidates, Candidate{Path: r.Path, Title: docTitle(r), Tags: r.Tags, Why: why, Fetch: true})
		related = slices.DeleteFunc(related, func(s string) bool { return strings.HasPrefix(s, r.Path+": ") })
	}
	return candidates, related
}

// Check is the dry_run report: the draft against the world's policy and
// style, the dedup findings, and the packet that binds apply to it.
type Check struct {
	Draft      Draft
	Policy     *publishpolicy.Policy // nil: the world has none
	Problems   []string              // style findings on the draft
	Missing    []string              // what the policy requires and the draft lacks
	Candidates []Candidate
	Related    []string // lookup rows that are not candidates, for the agent's judgement
	Notes      []string
	Packet     string
}

// Examine runs the gate and the policy on the draft. Nothing is fetched.
func Examine(d *Draft, policy *publishpolicy.Policy) *Check {
	c := &Check{Draft: *d, Policy: policy, Problems: gate.Problems(d.Path, d.Body, d.Metadata["description"]), Missing: Missing(d.Path, d.Metadata, policy)}
	c.Packet = docwrite.Digest(d.Path, strconv.Itoa(d.Version), d.Body, metaDigest(d.Metadata), d.Source)
	return c
}

// Missing words the policy's violations on the metadata, each a whole
// finding, as the server would at commit: after its type default, and
// never asking for identity keys, which are the surface's.
func Missing(path string, meta map[string]string, policy *publishpolicy.Policy) []string {
	if policy == nil {
		return nil
	}
	var out []string
	for _, v := range publishpolicy.EvaluateStrings(policy.ForCaller(), path, storefmt.ApplyOKFTypeDefault(path, meta)).Violations {
		switch v.Code {
		case publishpolicy.MissingTagAxis:
			out = append(out, "missing tag axis "+v.Name+" (an "+v.Name+":<value> tag)")
		case publishpolicy.MissingField:
			out = append(out, "missing metadata "+v.Name)
		case publishpolicy.DisallowedTagValue:
			out = append(out, "disallowed tag "+v.Name+" (allowed "+v.Axis+" values: "+policy.AllowedList(v.Axis)+")")
		case publishpolicy.UnpinnedSource:
			out = append(out, "unpinned "+provenance.Key+" value "+v.Name+" (pin "+provenance.PinForms+")")
		default:
			out = append(out, string(v.Code))
		}
	}
	return out
}

// Candidates picks the rows the draft may duplicate: the target path, the
// same title, and with byTags a row sharing two or more subject tags. One
// candidate per path; the rest are related.
func Candidates(rows []lookuptable.Row, d *Draft, byTags bool) (candidates []Candidate, related []string) {
	name, tags := strings.ToLower(d.Title()), d.SubjectTags()
	seen := map[string]bool{}
	for i := range rows {
		r := &rows[i]
		if seen[r.Path] {
			continue
		}
		seen[r.Path] = true
		rowTitle := docTitle(*r)
		c := Candidate{Path: r.Path, Title: rowTitle, Tags: r.Tags}
		switch shared := sharedTags(r.Tags, tags); {
		case r.Path == d.Path:
			c.Why, c.Fetch = "the target path", true
		case name != "" && strings.EqualFold(strings.TrimSpace(rowTitle), name):
			c.Why, c.Fetch = "same title", true
		case byTags && len(shared) >= 2:
			c.Why = "shares tags " + strings.Join(shared, ", ")
		default:
			related = append(related, r.Path+": "+rowTitle)
			continue
		}
		candidates = append(candidates, c)
	}
	return candidates, related
}

func sharedTags(rowTags string, tags []string) []string {
	var out []string
	for _, t := range protocol.SplitTags(rowTags) {
		if slices.Contains(tags, t) {
			out = append(out, t)
		}
	}
	return out
}

// Report renders the dry_run answer: the facts, then every fetched candidate.
func (c *Check) Report() string {
	var b strings.Builder
	status := "ready"
	if len(c.Problems) > 0 || len(c.Missing) > 0 {
		status = "findings"
	}
	fmt.Fprintf(&b, "status: %s\nversion: %d\n", status, c.Draft.Version)
	if c.Policy == nil {
		b.WriteString("policy: none\n")
	} else {
		fmt.Fprintf(&b, "policy: strictness %s", c.Policy.EffectiveStrictness())
		if len(c.Policy.RequiredTagAxes) > 0 {
			fmt.Fprintf(&b, "; require_tags %s", strings.Join(c.Policy.RequiredTagAxes, ", "))
		}
		if fields := c.Policy.ForCaller().RequiredFields; len(fields) > 0 {
			fmt.Fprintf(&b, "; require_fields %s", strings.Join(fields, ", "))
		}
		for _, axis := range slices.Sorted(maps.Keys(c.Policy.AllowedTagValues)) {
			fmt.Fprintf(&b, "; allow_tags.%s %s", axis, c.Policy.AllowedList(axis))
		}
		b.WriteString("\n")
	}
	if len(c.Missing) > 0 || len(c.Problems) > 0 {
		b.WriteString("findings:\n")
		for _, m := range c.Missing {
			b.WriteString("- " + m + "\n")
		}
		for _, p := range c.Problems {
			b.WriteString("- " + p + "\n")
		}
	}
	if len(c.Candidates) > 0 {
		b.WriteString("candidates:\n")
		for i := range c.Candidates {
			cd := &c.Candidates[i]
			fmt.Fprintf(&b, "- %s: %s", cd.Path, cd.Why)
			if cd.Version > 0 {
				fmt.Fprintf(&b, " (v%d)", cd.Version)
			}
			if cd.Tags != "" {
				fmt.Fprintf(&b, "; tags %s", cd.Tags)
			}
			b.WriteString("\n")
		}
	}
	if len(c.Related) > 0 {
		b.WriteString("related:\n")
		for _, r := range c.Related {
			b.WriteString("- " + r + "\n")
		}
	}
	for _, n := range c.Notes {
		b.WriteString("note: " + n + "\n")
	}
	fmt.Fprintf(&b, "packet: %s\n", c.Packet)
	for i := range c.Candidates {
		cd := &c.Candidates[i]
		if cd.Body != "" {
			fmt.Fprintf(&b, "\n=== %s (v%d)\n%s", cd.Path, cd.Version, cd.Body)
		}
	}
	return b.String()
}
