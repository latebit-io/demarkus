// Package docframe frames document text for a model, so the reader can tell
// where a document ends and the surface's own text resumes.
package docframe

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/contentcheck"
)

// Frame wraps document text for the model. The id hashes the text: a body
// forging its own close tag would have to contain its hash.
func Frame(text string) string {
	sum := sha256.Sum256([]byte(text))
	id := hex.EncodeToString(sum[:8])
	var b strings.Builder
	b.Grow(len(text) + 2*len("<doc-"+id+">\n") + 2)
	b.WriteString("<doc-" + id + ">\n")
	b.WriteString(text)
	if !strings.HasSuffix(text, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("</doc-" + id + ">\n")
	return b.String()
}

var frameLine = regexp.MustCompile(`(?m)^</?doc-[0-9a-f]{16}>$`)

// HasFrame reports a body carrying a Frame tag line: a fetch result
// pasted back whole instead of the text inside it.
func HasFrame(body string) bool {
	return strings.Contains(body, "doc-") && frameLine.MatchString(body)
}

// Flags are the lines that mark text read under meta: its read warning and a
// flagged classifier verdict, each newline-terminated; "" when neither.
func Flags(text string, meta map[string]string) string {
	var b strings.Builder
	if w := contentcheck.ReadWarning(text, meta); w != "" {
		b.WriteString(contentcheck.WarningKey + ": " + w + "\n")
	}
	if protocol.ClassifierFlagged(meta) {
		b.WriteString(protocol.ClassifierKey + ": " + meta[protocol.ClassifierKey] + "\n")
	}
	return b.String()
}
