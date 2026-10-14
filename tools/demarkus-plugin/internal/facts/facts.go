// Package facts computes where a session stopped (branch, uncommitted paths,
// check outcomes) for the agent to journal from. It never writes to a soul
// (ADR 0042): the agent decides what, if anything, to record.
package facts

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Check is a recognized kind of verification command.
type Check string

// The recognized check kinds.
const (
	Test      Check = "test"
	Lint      Check = "lint"
	Build     Check = "build"
	PreCommit Check = "pre-commit"
)

// checkOrder fixes the render order.
var checkOrder = []Check{Test, Lint, Build, PreCommit}

// Outcome is the last result of a check kind in the session. Unknown: the
// exit status does not speak for the check (piped, ; or || separated, or one
// of several checks in a failed chain).
type Outcome string

// The outcomes a check run can have.
const (
	Passed  Outcome = "passed"
	Failed  Outcome = "failed"
	Unknown Outcome = "unknown"
)

// Session is what the facts block renders. A nil Git means the git state was
// unavailable; a nil Checks means no transcript was read.
type Session struct {
	Git    *Git
	Checks map[Check]Outcome
}

// Git is the repository state at the moment the facts were read.
type Git struct {
	Root        string // repository top-level directory
	Branch      string // "" when detached
	Head        string // commit hash, shortened on render; "" before the first commit
	Uncommitted []string
}

const (
	maxPaths  = 20
	maxBlock  = 2048
	shortHash = 8
)

// checkPrefixes match a command segment's leading tokens (path tokens compared
// by base name, so ./pre-commit.sh and /usr/bin/go match).
var checkPrefixes = []struct {
	tokens []string
	check  Check
}{
	{[]string{"go", "test"}, Test},
	{[]string{"npm", "test"}, Test},
	{[]string{"npm", "run", "test"}, Test},
	{[]string{"pnpm", "test"}, Test},
	{[]string{"yarn", "test"}, Test},
	{[]string{"bun", "test"}, Test},
	{[]string{"cargo", "test"}, Test},
	{[]string{"pytest"}, Test},
	{[]string{"python", "-m", "pytest"}, Test},
	{[]string{"python3", "-m", "pytest"}, Test},
	{[]string{"make", "test"}, Test},
	{[]string{"npx", "vitest"}, Test},
	{[]string{"npx", "jest"}, Test},
	{[]string{"golangci-lint"}, Lint},
	{[]string{"go", "vet"}, Lint},
	{[]string{"npm", "run", "lint"}, Lint},
	{[]string{"pnpm", "lint"}, Lint},
	{[]string{"yarn", "lint"}, Lint},
	{[]string{"cargo", "clippy"}, Lint},
	{[]string{"ruff"}, Lint},
	{[]string{"npx", "eslint"}, Lint},
	{[]string{"make", "lint"}, Lint},
	{[]string{"go", "build"}, Build},
	{[]string{"npm", "run", "build"}, Build},
	{[]string{"pnpm", "build"}, Build},
	{[]string{"yarn", "build"}, Build},
	{[]string{"cargo", "build"}, Build},
	{[]string{"make", "build"}, Build},
	{[]string{"pre-commit.sh"}, PreCommit},
	{[]string{"pre-commit", "run"}, PreCommit},
}

// Command is what a Bash command runs, as far as its exit status can tell.
type Command struct {
	Checks []Check // in order, each once
	// Attributable: an && chain with no piped check, so exit 0 means every
	// check passed and a failure belongs to one of them.
	Attributable bool
}

// Classify reads the checks a Bash command runs.
func Classify(command string) Command {
	segs, andOnly := splitSegments(command)
	cmd := Command{Attributable: andOnly}
	seen := map[Check]bool{}
	for _, seg := range segs {
		c, ok := classifySegment(strings.Fields(seg.stage))
		if !ok {
			continue
		}
		if seg.piped {
			cmd.Attributable = false
		}
		if !seen[c] {
			seen[c] = true
			cmd.Checks = append(cmd.Checks, c)
		}
	}
	return cmd
}

// Outcome is what an exit status says about the command's checks.
func (c Command) Outcome(exitOK bool) Outcome {
	switch {
	case !c.Attributable:
		return Unknown
	case exitOK:
		return Passed
	case len(c.Checks) == 1:
		return Failed
	default:
		return Unknown
	}
}

func classifySegment(tokens []string) (Check, bool) {
	for len(tokens) > 0 && isEnvAssignment(tokens[0]) {
		tokens = tokens[1:]
	}
	if len(tokens) > 1 && (tokens[0] == "bash" || tokens[0] == "sh") {
		tokens = tokens[1:] // a script run through a shell is the script
	}
	if len(tokens) == 1 && tokens[0] == "make" {
		return Build, true
	}
	for _, p := range checkPrefixes {
		if hasPrefix(tokens, p.tokens) {
			return p.check, true
		}
	}
	return "", false
}

func hasPrefix(tokens, prefix []string) bool {
	if len(tokens) < len(prefix) {
		return false
	}
	for i, want := range prefix {
		got := tokens[i]
		if strings.Contains(got, "/") {
			got = filepath.Base(got)
		}
		if got != want {
			return false
		}
	}
	return true
}

func isEnvAssignment(tok string) bool {
	i := strings.IndexByte(tok, '=')
	return i > 0 && !strings.ContainsAny(tok[:i], "-/.")
}

// segment is one command of a chain: its first pipeline stage, and whether a
// pipe follows it (then the exit status is the last stage's).
type segment struct {
	stage string
	piped bool
}

// splitSegments splits on &&, ||, ; and newlines outside quotes, so a check
// named inside a commit message is not a check, and a quoted | is no pipe.
// andOnly: every separator was &&.
func splitSegments(command string) (segs []segment, andOnly bool) {
	andOnly = true
	var cur strings.Builder
	var quote byte
	piped := false
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segs = append(segs, segment{stage: s, piped: piped})
		}
		cur.Reset()
		piped = false
	}
	write := func(c byte) {
		if !piped {
			cur.WriteByte(c)
		}
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			write(c)
		case c == '\'' || c == '"':
			quote = c
			write(c)
		case c == ';' || c == '\n':
			// A trailing ; or newline ends the command; it sequences nothing.
			andOnly = andOnly && strings.TrimSpace(command[i+1:]) == ""
			flush()
		case (c == '&' || c == '|') && i+1 < len(command) && command[i+1] == c:
			andOnly = andOnly && c == '&'
			flush()
			i++
		case c == '|':
			piped = true
		default:
			write(c)
		}
	}
	flush()
	return segs, andOnly
}

// Render is the facts block: a heading line and one bullet per fact, capped
// at maxPaths paths and maxBlock bytes. Empty when there is nothing to say.
func Render(s Session) string {
	var checks []string
	if s.Checks != nil {
		checks = []string{"checks: " + renderChecks(s.Checks)}
	}
	if s.Git == nil {
		if checks == nil {
			return ""
		}
		return block(checks...)
	}
	pos := s.Git.position()
	for n := min(len(s.Git.Uncommitted), maxPaths); ; n-- {
		lines := append([]string{pos, uncommittedLine(s.Git.Uncommitted, n)}, checks...)
		if b := block(lines...); len(b) <= maxBlock || n == 0 {
			return b
		}
	}
}

func block(lines ...string) string {
	return "Where it stopped:\n- " + strings.Join(lines, "\n- ") + "\n"
}

func (g *Git) position() string {
	pos := "branch: detached HEAD"
	if g.Branch != "" {
		pos = "branch: " + g.Branch
	}
	if g.Head == "" {
		pos += " (no commits yet)"
	} else {
		pos += " at " + g.Head[:min(len(g.Head), shortHash)]
	}
	if g.Root != "" {
		pos += " in " + filepath.Base(g.Root)
	}
	return pos
}

func uncommittedLine(paths []string, shown int) string {
	if len(paths) == 0 {
		return "uncommitted: none"
	}
	if shown == 0 {
		return fmt.Sprintf("uncommitted: %d paths", len(paths))
	}
	line := fmt.Sprintf("uncommitted (%d): %s", len(paths), strings.Join(paths[:shown], ", "))
	if rest := len(paths) - shown; rest > 0 {
		line += fmt.Sprintf(", +%d more", rest)
	}
	return line
}

func renderChecks(checks map[Check]Outcome) string {
	var known, unknown []string
	for _, c := range checkOrder {
		switch o, ok := checks[c]; {
		case !ok:
		case o == Unknown:
			unknown = append(unknown, string(c))
		default:
			known = append(known, string(c)+" "+string(o))
		}
	}
	if len(unknown) > 0 {
		known = append(known, strings.Join(unknown, ", ")+" ran, result not visible")
	}
	if len(known) == 0 {
		return "none recognized"
	}
	return strings.Join(known, "; ")
}
