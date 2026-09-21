package protocol

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
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

// MaxRequestPathLength keeps one path addressable by every protocol verb.
const MaxRequestPathLength = MaxRequestLineLength - len(VerbVersions) - 1

// MaxRequestFrontmatterLength is the maximum allowed size for request metadata.
const MaxRequestFrontmatterLength = 65536 // 64KB

// MaxBodyLength is the maximum allowed size for a document body (1 MiB).
const MaxBodyLength = 1 * 1024 * 1024

// requestWireOverhead is slack above the frontmatter and body budgets, so an
// oversized payload fails with a clean limit error instead of a truncated read.
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

	split, err := splitFrontmatterAndBody(rest)
	if err != nil {
		return Request{}, err
	}
	fm, body := split.Block, split.Body

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

// ErrMalformedRequest marks a request that breaks the wire grammar, as opposed
// to a read failure or a size limit. Servers answer it with bad-request.
var ErrMalformedRequest = errors.New("malformed request")

// parseRequestLine reads and validates the request line ("VERB /path\n").
func parseRequestLine(br *bufio.Reader) (verb, path string, err error) {
	line, err := readLineLimited(br, MaxRequestLineLength)
	if err != nil {
		return "", "", fmt.Errorf("reading request: %w", err)
	}

	verb, path, ok := strings.Cut(line, " ")
	if !ok {
		return "", "", fmt.Errorf("%w: %q", ErrMalformedRequest, line)
	}

	if verb == "" {
		return "", "", fmt.Errorf("%w: empty verb", ErrMalformedRequest)
	}
	if !IsValidVerb(verb) {
		return "", "", fmt.Errorf("%w: unknown verb: %q", ErrMalformedRequest, verb)
	}

	if err := ValidateRequestPath(path); err != nil {
		return "", "", err
	}

	return verb, path, nil
}

// ValidateRequestPath checks the path syntax shared by wire and storage layers.
func ValidateRequestPath(path string) error {
	if path == "" || !strings.HasPrefix(path, "/") {
		return fmt.Errorf("%w: invalid path: %q", ErrMalformedRequest, path)
	}
	if len(path) > MaxRequestPathLength {
		return fmt.Errorf("%w: invalid path: exceeds %d bytes", ErrMalformedRequest, MaxRequestPathLength)
	}
	if containsControlChars(path) {
		return fmt.Errorf("%w: invalid path: contains control characters", ErrMalformedRequest)
	}
	return nil
}

// readBoundedPayload reads everything after the request line, rejecting payloads
// that would exceed the combined frontmatter + body + delimiter budget.
func readBoundedPayload(br *bufio.Reader) ([]byte, error) {
	limit := int64(MaxRequestFrontmatterLength + MaxBodyLength + requestWireOverhead)
	rest, err := readBounded(br, limit, "request payload")
	if err != nil {
		return nil, err
	}
	return rest, nil
}

// readBounded reads r to EOF, erroring when the input exceeds limit bytes.
func readBounded(r io.Reader, limit int64, what string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds limit: %d bytes max", what, limit)
	}
	return data, nil
}

// splitFrontmatterAndBody splits a request payload and checks the frontmatter
// against MaxRequestFrontmatterLength; the caller checks the body length.
func splitFrontmatterAndBody(data []byte) (Frontmatter, error) {
	split, err := SplitFrontmatter(data)
	if err != nil {
		return Frontmatter{}, fmt.Errorf("%w: %w", ErrMalformedRequest, err)
	}
	if len(split.Block) > MaxRequestFrontmatterLength {
		return Frontmatter{}, fmt.Errorf("request metadata exceeds limit: %d > %d bytes", len(split.Block), MaxRequestFrontmatterLength)
	}
	return split, nil
}

// decodeFrontmatter parses a YAML frontmatter block into a string-to-string map.
// Always returns a non-nil map on success; YAML with no key-value pairs (e.g. a
// comment-only block) yields an empty map rather than nil.
func decodeFrontmatter(fm []byte) (map[string]string, error) {
	var meta map[string]string
	if err := yaml.Unmarshal(fm, &meta); err != nil {
		return nil, fmt.Errorf("%w: parsing request metadata: %w", ErrMalformedRequest, err)
	}
	if meta == nil {
		meta = make(map[string]string)
	}
	return meta, nil
}

// readLineLimited reads a single newline-terminated line from a bufio.Reader,
// rejecting any line that would exceed maxBytes before the full line is
// accumulated. Returns the line without the trailing newline. Returns io.EOF
// if no data is available.
func readLineLimited(br *bufio.Reader, maxBytes int) (string, error) {
	var line []byte
	for {
		fragment, isPrefix, err := br.ReadLine()
		if len(line)+len(fragment) > maxBytes {
			return "", fmt.Errorf("request line exceeds limit: %d > %d bytes", len(line)+len(fragment), maxBytes)
		}
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
		buf.WriteString(FrontmatterFence)
		buf.Write(yamlBytes)
		buf.WriteString(FrontmatterFence)
	} else if strings.HasPrefix(req.Body, FrontmatterFence) {
		// Empty block first, or the parser reads the body's own fence as metadata.
		buf.WriteString(FrontmatterFence + frontmatterClose)
	}

	if req.Body != "" {
		buf.WriteString(req.Body)
	}

	n, err := w.Write(buf.Bytes())
	return int64(n), err
}

// VersionPath is the immutable FETCH path of one document version.
func VersionPath(docPath string, version int) string {
	return docPath + "/v" + strconv.Itoa(version)
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
	case VerbFetch, VerbList, VerbVersions, VerbPublish, VerbArchive, VerbAppend, VerbLookup:
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
