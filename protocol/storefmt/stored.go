package storefmt

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
)

// ContentHash computes the sha256 content hash for a document body.
func ContentHash(body []byte) string {
	h := sha256.Sum256(body)
	return "sha256-" + hex.EncodeToString(h[:])
}

// storedField returns one loosely spelled header field of a stored version;
// the strict grammar lives in InspectStoredVersion.
func storedField(data []byte, key string) (string, bool) {
	split, ok := splitStored(data)
	if !ok {
		return "", false
	}
	for line := range strings.SplitSeq(string(split.Block), "\n") {
		name, value, found := strings.Cut(line, ": ")
		if found && strings.TrimSpace(name) == key {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// ExtractPreviousHash returns the previous-hash chain link, or "" if absent.
func ExtractPreviousHash(data []byte) string {
	value, _ := storedField(data, "previous-hash")
	return value
}

// IsArchived reports the legacy archived flag in stored frontmatter.
func IsArchived(data []byte) bool {
	value, _ := storedField(data, "archived")
	return value == "true"
}

// JoinContent appends content, adding a newline only when existing lacks one.
// It returns ErrSizeLimit past protocol.MaxBodyLength.
func JoinContent(existing, content []byte) ([]byte, error) {
	if len(content) == 0 {
		return existing, nil
	}
	sep := 0
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		sep = 1
	}
	n := int64(len(existing)) + int64(sep) + int64(len(content))
	if n > protocol.MaxBodyLength {
		return nil, ErrSizeLimit
	}
	combined := make([]byte, 0, int(n))
	combined = append(combined, existing...)
	if sep == 1 {
		combined = append(combined, '\n')
	}
	combined = append(combined, content...)
	return combined, nil
}

// ExtractBody returns the content after the store frontmatter.
// If no frontmatter is found, the entire data is returned.
func ExtractBody(data []byte) []byte {
	split, ok := splitStored(data)
	if !ok {
		return data
	}
	return split.Body
}

// splitStored splits a stored version at its fences. The stored grammar needs a
// newline after the closing fence, so the wire's end of input form is refused.
func splitStored(data []byte) (protocol.Frontmatter, bool) {
	split, err := protocol.SplitFrontmatter(data)
	if err != nil || !split.Found || split.ClosedAtEOF {
		return protocol.Frontmatter{}, false
	}
	return split, true
}
