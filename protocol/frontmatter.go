package protocol

import (
	"bytes"
	"errors"
)

// Frontmatter delimiters, shared by the wire parsers, the serializers and the
// stored version format so no side can drift.
const (
	// FrontmatterFence is the bare fence line that opens and closes a block.
	FrontmatterFence = "---\n"
	// frontmatterClose is the closing fence with its preceding line break.
	frontmatterClose = "\n" + FrontmatterFence
	// frontmatterTrim is a closing fence at end of input, no trailing newline.
	frontmatterTrim = "\n---"
)

// ErrUnclosedFrontmatter reports an opening fence with no closing fence.
var ErrUnclosedFrontmatter = errors.New("unclosed frontmatter")

// Frontmatter is a payload split at its fences. Block and Body alias the input.
type Frontmatter struct {
	// Found is false when the input has no opening fence; Body is then the input.
	Found bool
	// Block is the text between the fences, without them.
	Block []byte
	// Body is everything after the closing fence.
	Body []byte
	// ClosedAtEOF marks the "\n---" at end of input form, which the stored
	// version grammar refuses and the wire accepts.
	ClosedAtEOF bool
}

// SplitFrontmatter splits data at the first closing fence.
func SplitFrontmatter(data []byte) (Frontmatter, error) {
	if !bytes.HasPrefix(data, []byte(FrontmatterFence)) {
		return Frontmatter{Body: data}, nil
	}
	inner := data[len(FrontmatterFence):]
	if block, body, ok := bytes.Cut(inner, []byte(frontmatterClose)); ok {
		return Frontmatter{Found: true, Block: block, Body: body}, nil
	}
	if bytes.HasSuffix(inner, []byte(frontmatterTrim)) {
		end := len(inner) - len(frontmatterTrim)
		return Frontmatter{Found: true, Block: inner[:end], ClosedAtEOF: true}, nil
	}
	return Frontmatter{}, ErrUnclosedFrontmatter
}
