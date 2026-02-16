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
