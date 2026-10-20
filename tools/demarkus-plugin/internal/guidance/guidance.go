// Package guidance assembles session start for every demarkus plugin: dynamic
// pieces (health warning, one-time offers, project status, joined systems)
// around generated static guidance, plus the user's status line.
package guidance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/broker"
	registryknowledge "github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/knowledge"
)

// Input selects which surface's session guidance to evaluate.
type Input struct {
	Surface      string `json:"surface"`      // memory | knowledge
	GuidanceFile string `json:"guidanceFile"` // path to the plugin's static guidance md
	// ProjectDir overrides the harness environment when the adapter passes one.
	ProjectDir string `json:"projectDir,omitempty"`
	// NoStatus: the host shows the user no status line, so a one-time notice
	// waits for a session on a host that does.
	NoStatus bool `json:"noStatus,omitempty"`
}

// Output carries the guidance text to inject for a session.
type Output struct {
	Context string `json:"context"`          // full text to inject; empty = nothing to say
	Status  string `json:"status,omitempty"` // one line for the user; empty = nothing to show
}

// brandNames are the words a plugin's status line uses. The generator writes
// context/brand.json beside a brand's guidance file; the base plugins have
// none and keep the demarkus words in baseBrand.
type brandNames struct {
	PluginName string `json:"plugin_name"`
	Store      string `json:"store_noun"`
}

var baseBrand = map[string]brandNames{
	"memory":    {PluginName: "demarkus", Store: "soul"},
	"knowledge": {PluginName: "demarkus-knowledge", Store: "soul"},
}

func readBrand(surface, guidanceFile string) (brandNames, error) {
	b := baseBrand[surface]
	if guidanceFile == "" {
		return b, nil
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(guidanceFile), "brand.json"))
	if errors.Is(err, os.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return brandNames{}, fmt.Errorf("read brand names: %w", err)
	}
	// Absent keys keep the base words.
	if err := json.Unmarshal(raw, &b); err != nil {
		return brandNames{}, fmt.Errorf("parse brand.json: %w", err)
	}
	return b, nil
}

// memoryOfferText follows the plugin name on the status line, never in agent
// context: raised by the agent, it landed inside unrelated replies.
const memoryOfferText = " is now your persistent memory store; to make it the only one, ask to disable the host's built-in memory tool"

// Evaluate builds the session guidance to inject for the requested surface.
func Evaluate(in Input) (Output, error) {
	switch in.Surface {
	case "knowledge":
		return knowledge(in)
	default:
		return memory(in)
	}
}

// readFile returns the static guidance without the generator's leading HTML
// comments, which would otherwise ride every session. An empty path is
// optional; a configured path that cannot be read is an error.
func readFile(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("read guidance file: %w", err)
	}
	text := string(b)
	for {
		line, rest, found := strings.Cut(text, "\n")
		if !found || !strings.HasPrefix(line, "<!--") || !strings.HasSuffix(line, "-->") {
			return strings.TrimLeft(text, "\n"), nil
		}
		text = rest
	}
}

func memory(in Input) (Output, error) {
	var parts []string
	warn, err := serverHealthWarning()
	if err != nil {
		return Output{}, err
	}
	if warn != "" {
		parts = append(parts, "⚠️ "+warn)
	}
	brand, err := readBrand("memory", in.GuidanceFile)
	if err != nil {
		return Output{}, err
	}
	header, status, err := projectHeader(in.ProjectDir, warn, brand)
	if err != nil {
		return Output{}, err
	}
	if header != "" {
		parts = append(parts, header)
	}
	// The offer waits for a session that shows the user a healthy status line.
	if warn == "" && status != "" && !in.NoStatus {
		offer, err := memoryOffer(brand)
		if err != nil {
			return Output{}, err
		}
		if offer != "" {
			status += " · " + offer
		}
	}
	g, err := readFile(in.GuidanceFile)
	if err != nil {
		return Output{}, err
	}
	if g != "" {
		parts = append(parts, g)
	}
	return Output{Context: strings.Join(parts, "\n\n"), Status: status}, nil
}

// projectHeader states the project, its root, state and where the work stands
// for the agent; status is the user's line. Both are empty without a project
// directory, or when an unbound project would name an unconfigured local store.
func projectHeader(dir, warn string, brand brandNames) (header, status string, err error) {
	r, err := project.Resolve(dir)
	if errors.Is(err, project.ErrNoDir) {
		return "", "", nil
	}
	if err != nil {
		var slugErr *project.SlugError
		if errors.As(err, &slugErr) {
			return "Project slug: none (" + slugErr.Error() + ").", slugStatus(slugErr, brand), nil
		}
		return "", "", err
	}
	if r.State == project.StateLocal {
		if configured, err := LocalConfigured(); err != nil || !configured {
			return "", "", err
		}
	}
	s := readStatus(r)
	return s.agentHeader(r), s.userLine(r, warn, brand), nil
}

// serverHealthWarning is provision.HealthWarning (the PID-at-root ownership
// test lives there alone); a variable so tests can stand in a healthy server.
var serverHealthWarning = provision.HealthWarning

// memoryOffer returns the one-time offer text the first time, "" thereafter.
func memoryOffer(brand brandNames) (string, error) {
	sentinel, err := config.StatePath(".memory-offer-shown")
	if err != nil {
		return "", err
	}
	if _, e := os.Stat(sentinel); e == nil {
		return "", nil // already shown
	}
	// Best effort: a missing sentinel only repeats the offer next session.
	_ = os.MkdirAll(filepath.Dir(sentinel), 0o755)
	_ = os.WriteFile(sentinel, nil, 0o644)
	return brand.PluginName + memoryOfferText, nil
}

// probeBroker is the broker reachability probe; tests replace it so none
// dials out.
var probeBroker = broker.Reach

// systemStatus is one joined system as the session start reports it: a
// probe error, or none with an empty url when the join recorded no url.
type systemStatus struct {
	slug, url string
	err       error
}

// probeSystems reaches every joined broker side by side within statusBudget.
func probeSystems(systems []string) ([]systemStatus, error) {
	out := make([]systemStatus, len(systems))
	ctx, cancel := context.WithTimeout(context.Background(), statusBudget)
	defer cancel()
	var wg sync.WaitGroup
	for i, slug := range systems {
		u, err := registryknowledge.URL(slug)
		if err != nil {
			return nil, err
		}
		out[i] = systemStatus{slug: slug, url: u}
		if u == "" {
			continue
		}
		wg.Go(func() { out[i].err = probeBroker(ctx, u) })
	}
	wg.Wait()
	return out, nil
}

// agentLine is the system's bullet in the session header.
func (s *systemStatus) agentLine() string {
	line := "- **" + s.slug + "** (MCP server `" + s.slug + "`"
	switch {
	case s.url == "":
		return line + "; broker URL not recorded, re-join to enable the reachability check)"
	case s.err != nil:
		return line + ", " + s.url + "): ⚠️ " + s.err.Error()
	}
	return line + ", " + s.url + "): broker reachable"
}

// userWord is the system's word on the status line.
func (s *systemStatus) userWord() string {
	switch {
	case s.url == "":
		return s.slug + " joined"
	case s.err != nil:
		return s.slug + " ⚠️ broker unreachable"
	}
	return s.slug + " reachable"
}

// knowledgeNotConnected tells the agent how an unreachable or unauthenticated
// system shows up: its tools are missing, so no shared read can run.
const knowledgeNotConnected = "A joined system whose `mark_*` tools are absent from this session is not connected (OAuth pending, or its MCP server not registered on this host): say so before answering a shared question, never answer from memory as if it were."

func knowledge(in Input) (Output, error) {
	systems, err := config.ListKnowledgeSystems()
	if err != nil {
		return Output{}, err
	}
	brand, err := readBrand("knowledge", in.GuidanceFile)
	if err != nil {
		return Output{}, err
	}
	if len(systems) == 0 {
		status := brand.PluginName + ": no system joined · /knowledge-join <broker-url>"
		// One-time pointer to /knowledge-join in the agent's context; the
		// status line says it every session.
		sentinel, err := config.StatePath(".knowledge-join-hint-shown")
		if err != nil {
			return Output{}, err
		}
		if _, e := os.Stat(sentinel); e == nil {
			return Output{Status: status}, nil
		}
		// Best effort: a missing sentinel only repeats the hint next session.
		_ = os.MkdirAll(filepath.Dir(sentinel), 0o755)
		_ = os.WriteFile(sentinel, nil, 0o644)
		return Output{Context: brand.PluginName + " is installed but no knowledge system is joined yet. To connect this installation to your organization's shared demarkus knowledge base, run `/knowledge-join <broker-url>`.", Status: status}, nil
	}
	states, err := probeSystems(systems)
	if err != nil {
		return Output{}, err
	}

	var b strings.Builder
	b.WriteString("# demarkus knowledge system(s) joined\n\n")
	b.WriteString("You are connected to the following organizational demarkus knowledge system(s), available as MCP servers (use `mark://<world>/<path>` URLs against them):\n")
	words := make([]string, 0, len(states))
	for i := range states {
		b.WriteString("\n" + states[i].agentLine())
		words = append(words, states[i].userWord())
	}
	b.WriteString("\n\n" + knowledgeNotConnected)
	status := brand.PluginName + ": " + strings.Join(words, " · ")
	parts := []string{b.String()}
	g, err := readFile(in.GuidanceFile)
	if err != nil {
		return Output{}, err
	}
	if g != "" {
		parts = append(parts, g)
	}
	memory, err := config.MemoryConfigured()
	if err != nil {
		return Output{}, err
	}
	if !memory {
		parts = append(parts, "(No local demarkus store is configured here, so the store↔system guidance above is informational only; everything durable goes to the knowledge system.)")
	}
	return Output{Context: strings.Join(parts, "\n\n"), Status: status}, nil
}

// LocalConfigured reports a usable plugin-memory.conf: the local store an
// unbound project would write to exists. An unreadable one is an error.
func LocalConfigured() (bool, error) {
	cfg, err := config.LoadConfig()
	return cfg != nil, err
}
