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
	Body     string
}

// MaxRequestLineLength is the maximum allowed length for a request line.
const MaxRequestLineLength = 4096

// MaxRequestFrontmatterLength is the maximum allowed size for request metadata.
const MaxRequestFrontmatterLength = 65536 // 64KB

// ParseRequest reads a request from r.
// Format: "VERB /path\n" followed by optional YAML frontmatter and body.
// The body is read as raw bytes to preserve content verbatim.
func ParseRequest(r io.Reader) (Request, error) {
	br := bufio.NewReader(r)

	// Read the request line.
	line, err := readLine(br)
	if err != nil {
		return Request{}, fmt.Errorf("reading request: %w", err)
	}
	if len(line) > MaxRequestLineLength {
		return Request{}, fmt.Errorf("request line exceeds limit: %d > %d bytes", len(line), MaxRequestLineLength)
	}

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

	// Peek at the next line to check for frontmatter.
	nextLine, err := readLine(br)
	if err != nil {
		// No more data after request line — that's fine.
		return req, nil
	}

	if nextLine == "---" {
		// Parse frontmatter lines until closing ---.
		var fmBuf strings.Builder
		closedFrontmatter := false
		for {
			fmLine, err := readLine(br)
			if err != nil {
				break
			}
			if fmLine == "---" {
				closedFrontmatter = true
				break
			}
			fmBuf.WriteString(fmLine)
			fmBuf.WriteByte('\n')

			if fmBuf.Len() > MaxRequestFrontmatterLength {
				return Request{}, fmt.Errorf("request metadata exceeds limit: %d > %d bytes", fmBuf.Len(), MaxRequestFrontmatterLength)
			}
		}
		if !closedFrontmatter {
			return Request{}, fmt.Errorf("malformed request: unclosed frontmatter")
		}
		if fmBuf.Len() > 0 {
			var raw map[string]string
			if err := yaml.Unmarshal([]byte(fmBuf.String()), &raw); err != nil {
				return Request{}, fmt.Errorf("parsing request metadata: %w", err)
			}
			req.Metadata = raw
		}
		// Read remaining bytes as body verbatim.
		body, err := io.ReadAll(br)
		if err != nil {
			return Request{}, fmt.Errorf("reading request body: %w", err)
		}
		req.Body = string(body)
	} else {
		// No frontmatter — first line plus remaining bytes are the body.
		body, err := io.ReadAll(br)
		if err != nil {
			return Request{}, fmt.Errorf("reading request body: %w", err)
		}
		req.Body = nextLine + "\n" + string(body)
	}

	return req, nil
}

// readLine reads a single newline-terminated line from a bufio.Reader,
// returning the line without the trailing newline. Returns io.EOF if no
// data is available.
func readLine(br *bufio.Reader) (string, error) {
	var line []byte
	for {
		fragment, isPrefix, err := br.ReadLine()
		line = append(line, fragment...)
		if err != nil {
			if len(line) > 0 {
				return string(line), nil
			}
			return "", err
		}
		if !isPrefix {
			return string(line), nil
		}
	}
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

	if req.Body != "" {
		buf.WriteString(req.Body)
	}

	n, err := w.Write(buf.Bytes())
	return int64(n), err
}

// isValidVerb returns true if verb is a known Mark Protocol verb.
func isValidVerb(verb string) bool {
	switch verb {
	case VerbFetch, VerbList, VerbVersions, VerbWrite:
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
