// Package guidance assembles the session-start context for every demarkus plugin
// — the dynamic pieces (managed-server health warning, the one-time "make
// demarkus your only memory" offer, the joined-knowledge-systems list, the
// one-time join hint, the soul↔system note) wrapped around the plugin's bundled
// static guidance markdown. The static prose stays per-plugin (passed in via
// guidanceFile); only the LOGIC lives here, shared across harnesses.
package guidance

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

type Input struct {
	Surface      string `json:"surface"`      // memory | knowledge
	GuidanceFile string `json:"guidanceFile"` // path to the plugin's static guidance md
}

type Output struct {
	Context string `json:"context"` // full text to inject; empty = nothing to say
}

const memoryOfferText = "[One-time setup offer — raise this with the user once, then drop it] demarkus-memory is now wired as your persistent memory store. If the user would like demarkus to be their SINGLE source of durable memory, offer to disable your harness's built-in memory tool so the two don't compete: ask first, and only if they agree, turn it off and report what you changed. If they decline, say nothing further about it. Do not disable anything unasked."

func Evaluate(in Input) (Output, error) {
	switch in.Surface {
	case "knowledge":
		return knowledge(in)
	default:
		return memory(in)
	}
}

func readFile(p string) string {
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
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
	if g := readFile(in.GuidanceFile); g != "" {
		parts = append(parts, g)
	}
	return Output{Context: strings.Join(parts, "\n\n")}, nil
}

// serverHealthWarning returns a one-line warning when the managed server doesn't
// look alive, "" when healthy/indeterminate. Managed (default/isolated) modes
// only; reuse is left to /soul-status.
func serverHealthWarning() (string, error) {
	cfg, err := config.LoadConfig()
	if err != nil || cfg == nil {
		return "", err
	}
	if cfg.Mode != "default" && cfg.Mode != "isolated" {
		return "", nil
	}
	pidStr := strings.TrimSpace(readFile(filepath.Join(cfg.SoulDir, ".pid")))
	alive := false
	if pid, e := strconv.Atoi(pidStr); e == nil && pid > 0 {
		if p, e2 := os.FindProcess(pid); e2 == nil {
			alive = p.Signal(syscall.Signal(0)) == nil
		}
	}
	if alive {
		return "", nil
	}
	return "the demarkus-memory server is not running (no live process for " + cfg.SoulDir +
		"). Memory tools (mark_fetch/mark_publish/mark_lookup/...) will fail until it restarts — run /soul-init to restart, or /soul-status to diagnose.", nil
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
	_ = os.MkdirAll(filepath.Dir(sentinel), 0o755)
	_ = os.WriteFile(sentinel, nil, 0o644) // best-effort; re-offer beats hard-fail
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
	if g := readFile(in.GuidanceFile); g != "" {
		parts = append(parts, g)
	}
	soul, err := config.SoulConfigured()
	if err != nil {
		return Output{}, err
	}
	if !soul {
		parts = append(parts, "(No local demarkus soul is configured here, so the soul↔system guidance above is informational only — everything durable goes to the knowledge system.)")
	}
	return Output{Context: strings.Join(parts, "\n\n")}, nil
}
