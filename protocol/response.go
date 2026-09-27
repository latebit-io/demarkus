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

	// WATCH terminal statuses (§6.8): the cursor cannot be resumed from, or the
	// server is draining. Both end the stream.
	StatusResync  = "resync"
	StatusClosing = "closing"
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

	status, meta, err := decodeStatusBlock(split.Block)
	if err != nil {
		return Response{}, fmt.Errorf("parsing frontmatter: %w", err)
	}
	resp.Status, resp.Metadata = status, meta
	return resp, nil
}

// WriteTo writes the response to w in wire format.
func (resp Response) WriteTo(w io.Writer) (int64, error) {
	block, err := encodeStatusBlock(resp.Status, resp.Metadata)
	if err != nil {
		return 0, fmt.Errorf("encoding frontmatter: %w", err)
	}
	n, err := w.Write(append(block, resp.Body...))
	return int64(n), err
}

// encodeStatusBlock is a fenced frontmatter block of meta plus status; an
// empty status is left out. Response and WATCH blocks share it.
func encodeStatusBlock(status string, meta map[string]string) ([]byte, error) {
	fm := make(map[string]string, len(meta)+1)
	maps.Copy(fm, meta)
	if status != "" {
		fm["status"] = status
	}
	var buf bytes.Buffer
	buf.WriteString(FrontmatterFence)
	if len(fm) > 0 {
		yamlBytes, err := yaml.Marshal(fm)
		if err != nil {
			return nil, err
		}
		buf.Write(yamlBytes)
	}
	buf.WriteString(FrontmatterFence)
	return buf.Bytes(), nil
}

// decodeStatusBlock parses the text between the fences into the status and
// the other keys, as strings so YAML reads no timestamps or numbers.
func decodeStatusBlock(block []byte) (status string, meta map[string]string, err error) {
	var raw map[string]string
	if err := yaml.Unmarshal(block, &raw); err != nil {
		return "", nil, err
	}
	meta = make(map[string]string, len(raw))
	for k, v := range raw {
		if k == "status" {
			status = v
		} else {
			meta[k] = v
		}
	}
	return status, meta, nil
}
