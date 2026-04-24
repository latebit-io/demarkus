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

// MaxBodyLength is the maximum allowed size for a document body (1 MiB).
const MaxBodyLength = 1 * 1024 * 1024

// Frontmatter delimiters as they appear on the wire. Parser and serializer
// share these so the two sides of the wire format cannot drift.
const (
	// frontmatterFence is the bare fence line that opens and closes a
	// YAML frontmatter block on the wire.
	frontmatterFence = "---\n"
	// frontmatterOpen is the opening delimiter at the start of a request
	// payload (no leading newline).
	frontmatterOpen = frontmatterFence
	// frontmatterClose is the closing delimiter with a body following
	// (leading newline from prior content + fence).
	frontmatterClose = "\n" + frontmatterFence
	// frontmatterTrim matches a closing "---" at end-of-input, no trailing newline.
	frontmatterTrim = "\n---"
)

// requestWireOverhead is generous slack allowed above the frontmatter and body
// limits when reading the request payload, so we get a clean
// "payload exceeds limit" error rather than truncating mid-read. The real
// budgets are MaxRequestFrontmatterLength and MaxBodyLength; this covers
// delimiter bytes plus any trailing bytes the client may send.
const requestWireOverhead = 64

// ParseRequest reads a request from r.
// Format: "VERB /path\n" followed by optional YAML frontmatter and body.
// The body is read as raw bytes to preserve content verbatim.
func ParseRequest(r io.Reader) (Request, error) {
	br := bufio.NewReader(r)

	verb, path, err := parseRequestLine(br)
	if err != nil {
		return Request{}, err
	}

	req := Request{Verb: verb, Path: path, Metadata: make(map[string]string)}

	rest, err := readBoundedPayload(br)
	if err != nil {
		return Request{}, err
	}
	if len(rest) == 0 {
		return req, nil
	}

	fm, body, err := splitFrontmatterAndBody(rest)
	if err != nil {
		return Request{}, err
	}

	if len(fm) > 0 {
		meta, err := decodeFrontmatter(fm)
		if err != nil {
			return Request{}, err
		}
		req.Metadata = meta
	}

	if len(body) > MaxBodyLength {
		return Request{}, fmt.Errorf("body exceeds limit: %d > %d bytes", len(body), MaxBodyLength)
	}
	req.Body = string(body)

	return req, nil
}

// parseRequestLine reads and validates the request line ("VERB /path\n").
func parseRequestLine(br *bufio.Reader) (verb, path string, err error) {
	line, err := readLine(br)
	if err != nil {
		return "", "", fmt.Errorf("reading request: %w", err)
	}
	if len(line) > MaxRequestLineLength {
		return "", "", fmt.Errorf("request line exceeds limit: %d > %d bytes", len(line), MaxRequestLineLength)
	}

	verb, path, ok := strings.Cut(line, " ")
	if !ok {
		return "", "", fmt.Errorf("malformed request: %q", line)
	}

	if verb == "" {
		return "", "", fmt.Errorf("empty verb")
	}
	if !IsValidVerb(verb) {
		return "", "", fmt.Errorf("unknown verb: %q", verb)
	}

	if path == "" || !strings.HasPrefix(path, "/") {
		return "", "", fmt.Errorf("invalid path: %q", path)
	}
	if containsControlChars(path) {
		return "", "", fmt.Errorf("invalid path: contains control characters")
	}

	return verb, path, nil
}

// readBoundedPayload reads everything after the request line, rejecting payloads
// that would exceed the combined frontmatter + body + delimiter budget.
func readBoundedPayload(br *bufio.Reader) ([]byte, error) {
	limit := int64(MaxRequestFrontmatterLength + MaxBodyLength + requestWireOverhead)
	rest, err := io.ReadAll(io.LimitReader(br, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading request body: %w", err)
	}
	if int64(len(rest)) > limit {
		return nil, fmt.Errorf("request payload exceeds limit: %d bytes", len(rest))
	}
	return rest, nil
}

// splitFrontmatterAndBody separates a request payload into its frontmatter and
// body components. When no frontmatter is present the whole payload is the body.
// The frontmatter length is checked against MaxRequestFrontmatterLength here;
// the body length is checked by the caller against MaxBodyLength.
func splitFrontmatterAndBody(data []byte) (fm, body []byte, err error) {
	if !bytes.HasPrefix(data, []byte(frontmatterOpen)) {
		return nil, data, nil
	}

	inner := data[len(frontmatterOpen):]

	// Closing form 1: "\n---\n" with possibly more bytes after (the body).
	// Closing form 2: "\n---" at end of input, no body.
	var fmEnd, bodyStart int
	if idx := bytes.Index(inner, []byte(frontmatterClose)); idx >= 0 {
		fmEnd = idx
		bodyStart = idx + len(frontmatterClose)
	} else if bytes.HasSuffix(inner, []byte(frontmatterTrim)) {
		fmEnd = len(inner) - len(frontmatterTrim)
		bodyStart = len(inner)
	} else {
		return nil, nil, fmt.Errorf("malformed request: unclosed frontmatter")
	}

	fm = inner[:fmEnd]
	if len(fm) > MaxRequestFrontmatterLength {
		return nil, nil, fmt.Errorf("request metadata exceeds limit: %d > %d bytes", len(fm), MaxRequestFrontmatterLength)
	}
	body = inner[bodyStart:]
	return fm, body, nil
}

// decodeFrontmatter parses a YAML frontmatter block into a string-to-string map.
// Always returns a non-nil map on success; YAML with no key-value pairs (e.g. a
// comment-only block) yields an empty map rather than nil.
func decodeFrontmatter(fm []byte) (map[string]string, error) {
	var meta map[string]string
	if err := yaml.Unmarshal(fm, &meta); err != nil {
		return nil, fmt.Errorf("parsing request metadata: %w", err)
	}
	if meta == nil {
		meta = make(map[string]string)
	}
	return meta, nil
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
		buf.WriteString(frontmatterFence)
		buf.Write(yamlBytes)
		buf.WriteString(frontmatterFence)
	}

	if req.Body != "" {
		buf.WriteString(req.Body)
	}

	n, err := w.Write(buf.Bytes())
	return int64(n), err
}

// IsHashPath checks if path matches the content-addressed fetch format (sha256-<64hex>).
// Returns the hash string (without leading /) and true if valid.
func IsHashPath(path string) (hash string, ok bool) {
	clean := strings.TrimPrefix(path, "/")
	if len(clean) != 71 || !strings.HasPrefix(clean, "sha256-") {
		return "", false
	}
	for _, c := range clean[7:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return clean, true
}

// IsValidVerb returns true if verb is a known Mark Protocol verb.
func IsValidVerb(verb string) bool {
	switch verb {
	case VerbFetch, VerbList, VerbVersions, VerbPublish, VerbArchive, VerbAppend:
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
