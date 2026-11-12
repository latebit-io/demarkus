package taint

import (
	"regexp"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/facts"
)

// A loopback URL as curl takes it: no userinfo, glob or backslash, which
// would name another host. localhost.evil.example is not one.
var loopbackURL = regexp.MustCompile(`(?i)^(?:https?://)?(?:localhost|127\.0\.0\.1|\[::1\])(?::\d{1,5})?(?:[/?#][^\s{}\[\]\\@]*)?$`)

// A redirection word (2>&1, >/dev/null). A spaced target (> file) is the
// next word, an operand like any other: not a loopback URL, so not exempt.
var redirection = regexp.MustCompile(`^(?:\d*|&)[<>]`)

// Ways a command sends a loopback curl elsewhere with nothing in its
// arguments to show it: a proxy variable, a curlrc home, another PATH, a
// function or alias named curl. Matched anywhere in the command, quoted or not.
var rerouted = regexp.MustCompile(`\b(?:(?:https?|all|no)_proxy|(?:HTTPS?|ALL|NO)_PROXY|CURL_HOME)\b|\b(?:(?:XDG_CONFIG_)?HOME|PATH)=|\bcurl\s*\(\s*\)|\bfunction\s+curl\b|\balias\s+(?:-\w+\s+)*curl=`)

// reroutes also reads the command bare, so a name split by quotes or an
// escaped newline is found.
func reroutes(command string) bool {
	return rerouted.MatchString(command) || rerouted.MatchString(facts.Bare(command))
}

// curl options that leave the request on the named host. Anything else
// (-L, --proxy, -K, --connect-to, --resolve, --url) is not vouched for.
const (
	curlFlags  = "sSfiIvkN"
	curlValues = "XHdowmAu"
)

var curlLong = map[string]bool{ // true: takes a value
	"silent": false, "show-error": false, "fail": false, "fail-with-body": false, "include": false, "head": false,
	"verbose": false, "insecure": false, "compressed": false, "no-buffer": false, "no-progress-meter": false,
	"request": true, "header": true, "data": true, "data-raw": true, "data-binary": true, "data-urlencode": true,
	"json": true, "output": true, "write-out": true, "max-time": true, "connect-timeout": true, "user-agent": true,
	"user": true, "retry": true, "retry-delay": true,
}

// localCurl reports a curl run that reads this machine only: every operand a
// loopback URL, every option a known one. curl alone: it follows no redirect
// unasked, wget and aria2c do. taint.ts localCurl is the same rule.
func localCurl(r facts.Run) bool {
	// A variable set for the run (PATH, HOME, a proxy) can move it; an
	// expansion anywhere means the words are not what curl gets.
	if r.Program != "curl" || r.Fed || r.Env || slices.ContainsFunc(r.Args, func(w string) bool { return strings.ContainsAny(w, "$`") }) {
		return false
	}
	urls := 0
	for i := 0; i < len(r.Args); i++ {
		w := r.Args[i]
		value := false
		switch {
		case redirection.MatchString(w):
		case strings.HasPrefix(w, "--"):
			takes, known := curlLong[w[2:]]
			if !known {
				return false
			}
			value = takes
		case len(w) > 1 && w[0] == '-':
			// A cluster: flags, then at most one option whose value is the
			// cluster's remainder or, when none, the next word.
			j := strings.IndexFunc(w[1:], func(r rune) bool { return !strings.ContainsRune(curlFlags, r) }) + 1
			if j > 0 && !strings.ContainsRune(curlValues, rune(w[j])) {
				return false
			}
			value = j == len(w)-1
		case loopbackURL.MatchString(w):
			urls++
		default:
			return false
		}
		if value {
			i++
			if i >= len(r.Args) {
				return false
			}
		}
	}
	return urls > 0
}
