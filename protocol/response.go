package protocol

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"

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
	StatusBadRequest   = "bad-request"
	StatusServerError  = "server-error"
	StatusRateLimited  = "rate-limited"
)

// ErrOutcomeUnknown marks a write whose request was sent and whose answer was
// lost: it may or may not have landed. A caller reconciles against the head,
// never resends. Wrap it with %w so errors.Is still finds it.
var ErrOutcomeUnknown = errors.New("request sent but outcome unknown")

// MaxResponseLength bounds a response read so a misbehaving server cannot
// OOM the client. Sized to hold a merge-conflict body carrying two full
// MaxBodyLength documents plus markers, frontmatter, and generated listings.
const MaxResponseLength = 4 * MaxBodyLength

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
	data, err := readBounded(r, MaxResponseLength, "response")
	if err != nil {
		return Response{}, err
	}

	resp := Response{Metadata: make(map[string]string)}

	split, err := SplitFrontmatter(data)
	if err != nil {
		return Response{}, fmt.Errorf("malformed frontmatter: missing closing ---: %w", err)
	}
	resp.Body = string(split.Body)
	if !split.Found || len(bytes.TrimSpace(split.Block)) == 0 {
		return resp, nil
	}

	// Parse as map[string]string to avoid YAML interpreting timestamps, numbers, etc.
	var raw map[string]string
	if err := yaml.Unmarshal(split.Block, &raw); err != nil {
		return Response{}, fmt.Errorf("parsing frontmatter: %w", err)
	}
	for k, v := range raw {
		if k == "status" {
			resp.Status = v
		} else {
			resp.Metadata[k] = v
		}
	}

	return resp, nil
}

// WriteTo writes the response to w in wire format.
func (resp Response) WriteTo(w io.Writer) (int64, error) {
	var buf bytes.Buffer

	fm := make(map[string]string, len(resp.Metadata)+1)
	maps.Copy(fm, resp.Metadata)
	fm["status"] = resp.Status

	yamlBytes, err := yaml.Marshal(fm)
	if err != nil {
		return 0, fmt.Errorf("encoding frontmatter: %w", err)
	}

	buf.WriteString(FrontmatterFence)
	buf.Write(yamlBytes)
	buf.WriteString(FrontmatterFence)

	if resp.Body != "" {
		buf.WriteString(resp.Body)
	}

	n, err := w.Write(buf.Bytes())
	return int64(n), err
}
