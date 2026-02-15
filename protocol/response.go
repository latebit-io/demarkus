package protocol

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Standard status values.
const (
	StatusOK          = "ok"
	StatusNotFound    = "not-found"
	StatusServerError = "server-error"
)

// Response represents a Mark Protocol response.
type Response struct {
	Status   string
	Metadata map[string]string
	Body     string
}

// ParseResponse reads a response from r.
// The response has optional YAML frontmatter delimited by "---" lines,
// followed by the markdown body.
func ParseResponse(r io.Reader) (Response, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Response{}, fmt.Errorf("reading response: %w", err)
	}

	content := string(data)
	resp := Response{Metadata: make(map[string]string)}

	if strings.HasPrefix(content, "---\n") {
		end := strings.Index(content[4:], "\n---\n")
		if end == -1 {
			return Response{}, fmt.Errorf("malformed frontmatter: missing closing ---")
		}

		fmData := content[4 : 4+end]
		// Parse as map[string]string to avoid YAML interpreting timestamps, numbers, etc.
		var raw map[string]string
		if err := yaml.Unmarshal([]byte(fmData), &raw); err != nil {
			return Response{}, fmt.Errorf("parsing frontmatter: %w", err)
		}

		for k, v := range raw {
			if k == "status" {
				resp.Status = v
			} else {
				resp.Metadata[k] = v
			}
		}

		resp.Body = content[4+end+5:] // skip past "\n---\n"
	} else {
		resp.Body = content
	}

	return resp, nil
}

// WriteTo writes the response to w in wire format.
func (resp Response) WriteTo(w io.Writer) (int64, error) {
	var buf bytes.Buffer

	buf.WriteString("---\n")
	buf.WriteString("status: " + resp.Status + "\n")

	keys := make([]string, 0, len(resp.Metadata))
	for k := range resp.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		buf.WriteString(k + ": " + resp.Metadata[k] + "\n")
	}
	buf.WriteString("---\n")

	if resp.Body != "" {
		buf.WriteString(resp.Body)
	}

	n, err := w.Write(buf.Bytes())
	return int64(n), err
}
