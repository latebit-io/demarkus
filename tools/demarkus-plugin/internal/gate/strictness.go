package gate

import (
	"os"
	"strings"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// Strictness is the enforcement level a guard applies to a violation: the
// policy's own levels, so a guard and a policy grade alike.
type Strictness = publishpolicy.Strictness

// The policy's levels, and Off for a guard that can be disabled (taint).
const (
	Warn             = publishpolicy.Warn
	Block            = publishpolicy.Block
	Ask              = publishpolicy.Ask
	Off   Strictness = "off"
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

// taintStrictness: env, the ~/.demarkus file, a brand's default, then warn (a
// routine gh api or web read must not prompt every write). quiet is warn
// without the line Claude Code shows the user; off drops the guard.
func taintStrictness() (s Strictness, quiet bool, err error) {
	v, err := config.Knob("DEMARKUS_TAINT_STRICTNESS", "plugin.taint-strictness")
	if err == nil && v == "" {
		var b config.Brand
		b, err = config.HookBrand()
		v = b.TaintStrictness
	}
	switch v = strings.TrimSpace(v); {
	case err != nil || v == "":
		return Warn, false, err
	case config.IsOff(v):
		return Off, false, nil
	case v == "quiet":
		return Warn, true, nil
	}
	// A set value that is no level (a typo, "ASK") must not weaken the guard.
	return validStrictness(v, Ask), false, nil
}

// knowledgeStrictness is the gate's severity on a joined knowledge system:
// the env override or warn. The server enforces the system's own policy.
func knowledgeStrictness() Strictness {
	return validStrictness(os.Getenv("DEMARKUS_KNOWLEDGE_STRICTNESS"), Warn)
}
