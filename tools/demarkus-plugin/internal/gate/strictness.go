package gate

import (
	"os"
	"strings"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// Strictness is the enforcement level a guard applies to a violation.
type Strictness string

// Enforcement levels a guard can apply to a violation.
const (
	Warn  Strictness = "warn"
	Block Strictness = "block"
	Ask   Strictness = "ask"
	// Off disables a guard that allows it (the taint guard).
	Off Strictness = "off"
)

func validStrictness(s string, def Strictness) Strictness {
	switch Strictness(strings.TrimSpace(s)) {
	case Warn, Block, Ask:
		return Strictness(strings.TrimSpace(s))
	default:
		return def
	}
}

// strictness reads a guard's level: env override, then the ~/.demarkus file,
// then def; an unreadable file is def with the error.
func strictness(env, file string, def Strictness) (Strictness, error) {
	v, err := config.Knob(env, file)
	return validStrictness(v, def), err
}

// memoryTagStrictness: the floor is warn.
func memoryTagStrictness() (Strictness, error) {
	return strictness("DEMARKUS_MEMORY_STRICTNESS", "plugin-memory.strictness", Warn)
}

// memoryDestStrictness: a misrouted write blocks by default.
func memoryDestStrictness() (Strictness, error) {
	return strictness("DEMARKUS_MEMORY_DEST_STRICTNESS", "plugin-memory.dest-strictness", Block)
}

// styleStrictness defaults to warn: style steers, it does not wall; the
// reasons teach the rule at the moment it is broken.
func styleStrictness() (Strictness, error) {
	return strictness("DEMARKUS_STYLE_STRICTNESS", "plugin.style-strictness", Warn)
}

// retentionStrictness defaults to ask: a prunable retention value deletes
// older versions permanently, so a human decides.
func retentionStrictness() (Strictness, error) {
	return strictness("DEMARKUS_RETENTION_STRICTNESS", "plugin.retention-strictness", Ask)
}

// governanceStrictness defaults to warn, which keeps an open world's flow
// prompt-free; ask puts a human on every policy write.
func governanceStrictness() (Strictness, error) {
	return strictness("DEMARKUS_GOVERNANCE_STRICTNESS", "plugin.governance-strictness", Warn)
}

// taintStrictness defaults to ask; off turns the guard off.
func taintStrictness() (Strictness, error) {
	v, err := config.Knob("DEMARKUS_TAINT_STRICTNESS", "plugin.taint-strictness")
	if err == nil && config.IsOff(v) {
		return Off, nil
	}
	return validStrictness(v, Ask), err
}

// knowledgeStrictness is the gate's severity on a joined knowledge system:
// the env override or warn. The server enforces the system's own policy.
func knowledgeStrictness() Strictness {
	return validStrictness(os.Getenv("DEMARKUS_KNOWLEDGE_STRICTNESS"), Warn)
}
