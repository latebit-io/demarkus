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

// wrappers run the command after their options and numeric values (timeout
// 10s, nice -n 5): the TS adapters' set.
var wrappers = map[string]bool{"sudo": true, "doas": true, "env": true, "exec": true, "nohup": true, "time": true,
	"nice": true, "timeout": true, "xargs": true, "stdbuf": true, "command": true}

// describes: command -v and -V (clustered too) name their operand, run nothing.
var describes = regexp.MustCompile(`^-[A-Za-z]*[vV]`)

// keywords precede a command and run nothing themselves: the TS adapters'
// set, and the brace that opens a group.
var keywords = map[string]bool{"if": true, "then": true, "elif": true, "else": true, "do": true, "while": true,
	"until": true, "!": true, "{": true}

// numericArg is a wrapper option value: 10, 2.5, 10s, 1m.
var numericArg = regexp.MustCompile(`^\d[0-9.]*[smhd]?$`)

// Runs names the first of programs some pipeline stage of command runs, or "":
// quotes respected, keywords, env assignments and wrappers (sudo -E) skipped,
// a path compared by its base name, a quoted name read bare ('wget' is wget).
func Runs(command string, programs ...[]string) string {
	segs, _ := splitSegments(command)
	for _, seg := range segs {
		for _, stage := range append([]string{seg.stage}, seg.later...) {
			tokens := strings.Fields(stage)
			for i, t := range tokens {
				tokens[i] = bare.Replace(t)
			}
			tokens = commandWords(tokens)
			if i := slices.IndexFunc(programs, func(p []string) bool { return hasPrefix(tokens, p) }); i >= 0 {
				return strings.Join(programs[i], " ")
			}
		}
	}
	return ""
}

// bare drops what the shell drops from a name: quotes, backslashes, an
// escaped newline.
var bare = strings.NewReplacer("\\\n", "", "'", "", `"`, "", `\`, "")

// commandWords drops what precedes the command a stage runs: keywords, a
// subshell's parenthesis, assignments, wrappers and the wrappers' options.
func commandWords(tokens []string) []string {
	for len(tokens) > 0 {
		if t := strings.TrimLeft(tokens[0], "("); t != tokens[0] {
			tokens = tokens[1:]
			if t != "" {
				tokens = append([]string{t}, tokens...)
			}
			continue
		}
		name := filepath.Base(tokens[0])
		switch {
		case keywords[tokens[0]] || isEnvAssignment(tokens[0]):
			tokens = tokens[1:]
		case wrappers[name]:
			tokens = tokens[1:]
			for len(tokens) > 0 && (strings.HasPrefix(tokens[0], "-") || numericArg.MatchString(tokens[0])) {
				if name == "command" && describes.MatchString(tokens[0]) {
					return nil
				}
				tokens = tokens[1:]
			}
		default:
			return tokens
		}
	}
	return tokens
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

// splitSegments splits on &&, ||, ;, & and newlines outside quotes: a check in
// a commit message is no check, a quoted | no pipe, a # comment runs nothing.
// andOnly: every separator was && (a background & never is: Bash returns first).
func splitSegments(command string) (segs []segment, andOnly bool) {
	andOnly = true
	var cur strings.Builder
	var quote byte
	var stages []string
	inWord := false // a # is a comment only where a word would start
	pipe := func() {
		stages = append(stages, strings.TrimSpace(cur.String()))
		cur.Reset()
		inWord = false
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
		case escapes(command, i, quote):
			// An escaped byte is part of its word, never a separator, quote
			// or comment; an escaped newline joins two lines.
			i++
			if command[i] != '\n' {
				write(c)
				write(command[i])
				inWord = true
			}
		case quote != 0:
			if c == quote {
				quote = 0
			}
			write(c)
		case c == '\'' || c == '"':
			quote, inWord = c, true
			write(c)
		case c == '#' && !inWord:
			i = lineEnd(command, i)
		case c == ';' || c == '\n':
			// A trailing ; or newline ends the command; it sequences nothing.
			andOnly = andOnly && strings.TrimSpace(command[i+1:]) == ""
			flush()
		case doubled(command, i):
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
			inWord = c != ' ' && c != '\t'
		}
	}
	flush()
	return segs, andOnly
}

// escapes: the backslash at i escapes the next byte (not inside single quotes).
func escapes(command string, i int, quote byte) bool {
	return command[i] == '\\' && quote != '\'' && i+1 < len(command)
}

// lineEnd is the index of the last byte of the line i is on.
func lineEnd(command string, i int) int {
	for i+1 < len(command) && command[i+1] != '\n' {
		i++
	}
	return i
}

// doubled: the byte at i starts && or ||.
func doubled(command string, i int) bool {
	c := command[i]
	return (c == '&' || c == '|') && i+1 < len(command) && command[i+1] == c
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
