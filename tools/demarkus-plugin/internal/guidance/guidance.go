// Package guidance assembles session start for every demarkus plugin: dynamic
// pieces (health warning, one-time offers, project status, joined systems)
// around generated static guidance, plus the user's status line.
package guidance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision"
)

// Input selects which surface's session guidance to evaluate.
type Input struct {
	Surface      string `json:"surface"`      // memory | knowledge
	GuidanceFile string `json:"guidanceFile"` // path to the plugin's static guidance md
	// ProjectDir overrides the harness environment when the adapter passes one.
	ProjectDir string `json:"projectDir,omitempty"`
}

// Output carries the guidance text to inject for a session.
type Output struct {
	Context string `json:"context"`          // full text to inject; empty = nothing to say
	Status  string `json:"status,omitempty"` // one line for the user; empty = nothing to show
}

const memoryOfferText = "[One-time setup offer: raise this with the user once, then drop it] demarkus-memory is now wired as your persistent memory store. If the user would like demarkus to be their SINGLE source of durable memory, offer to disable your harness's built-in memory tool so the two don't compete: ask first, and only if they agree, turn it off and report what you changed. If they decline, say nothing further about it. Do not disable anything unasked."

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
	offer, err := memoryOffer()
	if err != nil {
		return Output{}, err
	}
	if offer != "" {
		parts = append(parts, offer)
	}
	header, status, err := projectHeader(in.ProjectDir, warn)
	if err != nil {
		return Output{}, err
	}
	if header != "" {
		parts = append(parts, header)
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

// projectHeader states the project, its soul root and state, and where the
// work stands, as data for the agent; status is the user's line. Both empty
// when no project directory is known; an unusable directory name is stated.
func projectHeader(dir, warn string) (header, status string, err error) {
	r, err := project.Resolve(dir)
	if errors.Is(err, project.ErrNoDir) {
		return "", "", nil
	}
	if err != nil {
		var slugErr *project.SlugError
		if errors.As(err, &slugErr) {
			return "Project slug: none (" + slugErr.Error() + ").", slugStatus(slugErr), nil
		}
		return "", "", err
	}
	s := readStatus(r)
	return s.agentHeader(r), s.userLine(r, warn), nil
}

// serverHealthWarning delegates to provision.HealthWarning so the health check —
// including the PID-at-root ownership test that avoids a reused-.pid false
// positive — lives in exactly one place.
func serverHealthWarning() (string, error) {
	return provision.HealthWarning()
}

// memoryOffer returns the one-time offer text the first time, "" thereafter.
func memoryOffer() (string, error) {
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
	return memoryOfferText, nil
}

func knowledge(in Input) (Output, error) {
	systems, err := config.ListKnowledgeSystems()
	if err != nil {
		return Output{}, err
	}
	if len(systems) == 0 {
		// One-time pointer to /knowledge-join, then silent.
		sentinel, err := config.StatePath(".knowledge-join-hint-shown")
		if err != nil {
			return Output{}, err
		}
		if _, e := os.Stat(sentinel); e == nil {
			return Output{}, nil
		}
		// Best effort: a missing sentinel only repeats the hint next session.
		_ = os.MkdirAll(filepath.Dir(sentinel), 0o755)
		_ = os.WriteFile(sentinel, nil, 0o644)
		return Output{Context: "demarkus-knowledge is installed but no knowledge system is joined yet. To connect this installation to your organization's shared demarkus knowledge base, run `/knowledge-join <broker-url>`."}, nil
	}

	var b strings.Builder
	b.WriteString("# demarkus knowledge system(s) joined\n\n")
	b.WriteString("You are connected to the following organizational demarkus knowledge system(s), available as MCP servers (use `mark://<world>/<path>` URLs against them):\n")
	for _, s := range systems {
		b.WriteString("\n- **" + s + "** (MCP server `" + s + "`)")
	}
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
	return Output{Context: strings.Join(parts, "\n\n")}, nil
}
