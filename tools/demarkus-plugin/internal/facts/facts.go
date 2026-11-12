// Package facts computes where a session stopped (branch, uncommitted paths,
// check outcomes) for the agent to journal from. It never writes to a soul
// (ADR 0042): the agent decides what, if anything, to record.
package facts

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
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
	// Attributable: a foreground && chain with no piped check, so exit 0
	// means every check passed.
	Attributable bool
	// FailureIsCheck: the check is the whole command, so a failed exit is
	// its own; any other segment, even a cd, may be what failed.
	FailureIsCheck bool
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
	cmd.FailureIsCheck = cmd.Attributable && len(segs) == 1 && len(cmd.Checks) == 1
	return cmd
}

// Outcome is what an exit status says about the command's checks.
func (c Command) Outcome(exitOK bool) Outcome {
	switch {
	case !c.Attributable:
		return Unknown
	case exitOK:
		return Passed
	case c.FailureIsCheck:
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

// wrappers run the command after them, the TS adapters' set; true marks one
// whose options and numeric values (timeout 10s, nice -n 5) come first.
// command takes none: command -v curl runs nothing.
var wrappers = map[string]bool{"sudo": true, "doas": true, "env": true, "exec": true, "nohup": true, "time": true,
	"nice": true, "timeout": true, "xargs": true, "stdbuf": true, "command": false}

// numericArg is a wrapper option value: 10, 2.5, 10s, 1m.
var numericArg = regexp.MustCompile(`^\d[0-9.]*[smhd]?$`)

// Run is one of the programs asked about, as a pipeline stage runs it.
type Run struct {
	Program string   // as listed: "curl", "gh api"
	Args    []string // the words after it, unquoted; nil when a quote is unclosed
	Fed     bool     // under xargs, which appends operands the stage does not show
}

// Runs lists the programs the stages of command run, in order: quotes
// respected, env assignments and wrappers (sudo -E) skipped, a path compared
// by its base name.
func Runs(command string, programs ...[]string) []Run {
	var runs []Run
	segs, _ := splitSegments(command)
	for _, seg := range segs {
		for _, stage := range append([]string{seg.stage}, seg.later...) {
			tokens, fed := commandWords(strings.Fields(stage))
			i := slices.IndexFunc(programs, func(p []string) bool { return hasPrefix(tokens, p) })
			if i < 0 {
				continue
			}
			p := programs[i]
			runs = append(runs, Run{Program: strings.Join(p, " "), Args: words(strings.Join(tokens[len(p):], " ")), Fed: fed})
		}
	}
	return runs
}

// commandWords drops what precedes the command a stage runs: assignments,
// wrappers and the wrappers' options. fed: one of the wrappers was xargs.
func commandWords(tokens []string) (rest []string, fed bool) {
	for len(tokens) > 0 {
		name := filepath.Base(tokens[0])
		takesOptions, wrapper := wrappers[name]
		switch {
		case isEnvAssignment(tokens[0]):
			tokens = tokens[1:]
		case wrapper:
			fed = fed || name == "xargs"
			tokens = tokens[1:]
			for takesOptions && len(tokens) > 0 && (strings.HasPrefix(tokens[0], "-") || numericArg.MatchString(tokens[0])) {
				tokens = tokens[1:]
			}
		default:
			return tokens, fed
		}
	}
	return tokens, fed
}

// words splits s into shell words, quotes and backslashes removed; nil for
// an unclosed quote.
func words(s string) []string {
	out := []string{}
	var cur strings.Builder
	var quote byte
	open := false
	write := func(c byte) { _ = cur.WriteByte(c) } // a strings.Builder write never fails
	end := func() {
		if open {
			out = append(out, cur.String())
		}
		cur.Reset()
		open = false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == quote {
				quote = 0
			} else {
				write(c)
			}
		case quote == '"' && c == quote:
			quote = 0
		case c == '\\' && i+1 < len(s):
			i++
			write(s[i])
			open = true
		case quote == '"':
			write(c)
		case c == '\'' || c == '"':
			quote, open = c, true
		case c == ' ' || c == '\t':
			end()
		default:
			write(c)
			open = true
		}
	}
	if quote != 0 {
		return nil
	}
	end()
	return out
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
	stage string   // the pipeline's first stage
	piped bool     // stages follow it
	later []string // the stages after the first
}

// splitSegments splits on &&, ||, ;, & and newlines outside quotes, so a check
// named inside a commit message is not a check, and a quoted | is no pipe.
// andOnly: every separator was && (a background & never is: Bash returns first).
func splitSegments(command string) (segs []segment, andOnly bool) {
	andOnly = true
	var cur strings.Builder
	var quote byte
	var stages []string
	pipe := func() {
		stages = append(stages, strings.TrimSpace(cur.String()))
		cur.Reset()
	}
	flush := func() {
		pipe()
		if stages[0] != "" {
			segs = append(segs, segment{stage: stages[0], piped: len(stages) > 1, later: stages[1:]})
		}
		stages = nil
	}
	write := func(c byte) { _ = cur.WriteByte(c) } // a strings.Builder write never fails
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
			pipe()
			if i+1 < len(command) && command[i+1] == '&' {
				i++ // |& pipes stderr too
			}
		case c == '&' && !isRedirect(command, i):
			andOnly = false
			flush()
		default:
			write(c)
		}
	}
	flush()
	return segs, andOnly
}

// isRedirect: the & at i belongs to a redirection (2>&1, <&3, &>file).
func isRedirect(command string, i int) bool {
	return (i > 0 && (command[i-1] == '>' || command[i-1] == '<')) ||
		(i+1 < len(command) && command[i+1] == '>')
}

// Render is the facts block: heading, then one bullet per fact, capped at
// maxPaths paths and maxBlock bytes. Empty when there is nothing to say.
func Render(heading string, s Session) string {
	var checks []string
	if s.Checks != nil {
		checks = []string{"checks: " + renderChecks(s.Checks)}
	}
	if s.Git == nil {
		if checks == nil {
			return ""
		}
		return block(heading, checks...)
	}
	pos := s.Git.position()
	for n := min(len(s.Git.Uncommitted), maxPaths); ; n-- {
		lines := append([]string{pos, uncommittedLine(s.Git.Uncommitted, n)}, checks...)
		if b := block(heading, lines...); len(b) <= maxBlock || n == 0 {
			return b
		}
	}
}

func block(heading string, lines ...string) string {
	return heading + "\n- " + strings.Join(lines, "\n- ") + "\n"
}

// Brief is the one-line position for a user-facing status: branch, then the
// uncommitted count or clean.
func (g *Git) Brief() string {
	pos := g.Branch
	if pos == "" {
		pos = "detached HEAD"
	}
	if len(g.Uncommitted) == 0 {
		return pos + ", clean"
	}
	return fmt.Sprintf("%s, %d uncommitted", pos, len(g.Uncommitted))
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
