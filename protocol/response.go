package protocol

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"strings"

	"gopkg.in/yaml.v3"
)

// Standard status values.
const (
	StatusOK           = "ok"
	StatusCreated      = "created"
	StatusNotModified  = "not-modified"
	StatusNotFound     = "not-found"
	StatusArchived     = "archived"
	StatusUnauthorized = "unauthorized"
	StatusNotPermitted = "not-permitted"
	StatusConflict     = "conflict"
	StatusServerError  = "server-error"
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

		// Handle empty frontmatter gracefully
		if strings.TrimSpace(fmData) == "" {
			resp.Body = content[4+end+5:]
			return resp, nil
		}

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

	fm := make(map[string]string, len(resp.Metadata)+1)
	fm["status"] = resp.Status
	maps.Copy(fm, resp.Metadata)

	yamlBytes, err := yaml.Marshal(fm)
	if err != nil {
		return 0, fmt.Errorf("encoding frontmatter: %w", err)
	}

	buf.WriteString("---\n")
	buf.Write(yamlBytes)
	buf.WriteString("---\n")

	if resp.Body != "" {
		buf.WriteString(resp.Body)
	}

	n, err := w.Write(buf.Bytes())
	return int64(n), err
}
