package protocol

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Request represents a Mark Protocol request.
type Request struct {
	Verb     string
	Path     string
	Metadata map[string]string
}

// MaxRequestLineLength is the maximum allowed length for a request line.
const MaxRequestLineLength = 4096

// MaxRequestFrontmatterLength is the maximum allowed size for request metadata.
const MaxRequestFrontmatterLength = 65536 // 64KB

// ParseRequest reads a request from r.
// Format: "VERB /path\n" followed by optional YAML frontmatter.
func ParseRequest(r io.Reader) (Request, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024), MaxRequestLineLength)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return Request{}, fmt.Errorf("reading request: %w", err)
		}
		return Request{}, fmt.Errorf("reading request: unexpected EOF")
	}

	line := scanner.Text()
	verb, path, ok := strings.Cut(line, " ")
	if !ok {
		return Request{}, fmt.Errorf("malformed request: %q", line)
	}

	// Validate verb is non-empty and is a known verb
	if verb == "" {
		return Request{}, fmt.Errorf("empty verb")
	}
	if !isValidVerb(verb) {
		return Request{}, fmt.Errorf("unknown verb: %q", verb)
	}

	// Validate path is non-empty and starts with /
	if path == "" || !strings.HasPrefix(path, "/") {
		return Request{}, fmt.Errorf("invalid path: %q", path)
	}
	// Reject null bytes and control characters in paths.
	if containsControlChars(path) {
		return Request{}, fmt.Errorf("invalid path: contains control characters")
	}

	req := Request{Verb: verb, Path: path, Metadata: make(map[string]string)}

	// Check for optional frontmatter.
	if scanner.Scan() && scanner.Text() == "---" {
		var fmBuf strings.Builder
		for scanner.Scan() {
			if scanner.Text() == "---" {
				break
			}
			fmBuf.WriteString(scanner.Text())
			fmBuf.WriteByte('\n')

			// Enforce frontmatter size limit
			if fmBuf.Len() > MaxRequestFrontmatterLength {
				return Request{}, fmt.Errorf("request metadata exceeds limit: %d > %d bytes", fmBuf.Len(), MaxRequestFrontmatterLength)
			}
		}
		if err := scanner.Err(); err != nil {
			return Request{}, fmt.Errorf("reading request metadata: %w", err)
		}
		if fmBuf.Len() > 0 {
			var raw map[string]string
			if err := yaml.Unmarshal([]byte(fmBuf.String()), &raw); err != nil {
				return Request{}, fmt.Errorf("parsing request metadata: %w", err)
			}
			req.Metadata = raw
		}
	}

	return req, nil
}

// WriteTo writes the request to w in wire format.
func (req Request) WriteTo(w io.Writer) (int64, error) {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "%s %s\n", req.Verb, req.Path)

	if len(req.Metadata) > 0 {
		yamlBytes, err := yaml.Marshal(req.Metadata)
		if err != nil {
			return 0, fmt.Errorf("encoding request metadata: %w", err)
		}
		buf.WriteString("---\n")
		buf.Write(yamlBytes)
		buf.WriteString("---\n")
	}

	n, err := w.Write(buf.Bytes())
	return int64(n), err
}

// isValidVerb returns true if verb is a known Mark Protocol verb.
func isValidVerb(verb string) bool {
	switch verb {
	case VerbFetch, VerbList:
		return true
	default:
		return false
	}
}

// containsControlChars returns true if s contains null bytes or control characters
// (except tab, which is valid in paths on some systems).
func containsControlChars(s string) bool {
	for _, r := range s {
		if r == 0 || (r < 32 && r != '\t') || r == 127 {
			return true
		}
	}
	return false
}
