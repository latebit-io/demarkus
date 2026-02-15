package protocol

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Request represents a Mark Protocol request.
type Request struct {
	Verb string
	Path string
}

// MaxRequestLineLength is the maximum allowed length for a request line.
const MaxRequestLineLength = 4096

// ParseRequest reads a newline-terminated request line from r.
// Format: "VERB /path\n"
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

	return Request{Verb: verb, Path: path}, nil
}

// WriteTo writes the request to w in wire format.
func (req Request) WriteTo(w io.Writer) (int64, error) {
	n, err := fmt.Fprintf(w, "%s %s\n", req.Verb, req.Path)
	return int64(n), err
}
