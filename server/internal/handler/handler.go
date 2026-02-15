// Package handler serves Mark Protocol requests from a content directory.
package handler

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/latebit/demarkus/protocol"
)

// Handler serves markdown files from a content directory.
type Handler struct {
	ContentDir string
}

// Stream represents a bidirectional stream that can be read, written, and closed.
type Stream interface {
	io.ReadWriteCloser
}

// HandleStream reads a request from the stream and writes a response.
func (h *Handler) HandleStream(stream Stream) {
	defer stream.Close()

	req, err := protocol.ParseRequest(stream)
	if err != nil {
		log.Printf("bad request: %v", err)
		h.writeError(stream, protocol.StatusServerError, "bad request")
		return
	}

	log.Printf("%s %s", req.Verb, req.Path)

	if req.Verb != protocol.VerbFetch {
		h.writeError(stream, protocol.StatusServerError, "unsupported verb: "+req.Verb)
		return
	}

	h.handleFetch(stream, req.Path)
}

func (h *Handler) handleFetch(w io.Writer, reqPath string) {
	cleaned := filepath.Clean(reqPath)
	if strings.Contains(cleaned, "..") {
		h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
		return
	}

	filePath := filepath.Join(h.ContentDir, cleaned)

	info, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
		return
	}
	if err != nil {
		log.Printf("stat error: %v", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}
	if info.IsDir() {
		h.writeError(w, protocol.StatusNotFound, reqPath+" is a directory")
		return
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("read error: %v", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	body, existingMeta := stripFrontmatter(string(data))

	meta := map[string]string{
		"modified": info.ModTime().UTC().Format(time.RFC3339),
	}
	if v, ok := existingMeta["version"]; ok {
		meta["version"] = v
	} else {
		meta["version"] = "1"
	}

	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: meta,
		Body:     body,
	}
	resp.WriteTo(w)
}

func (h *Handler) writeError(w io.Writer, status, message string) {
	resp := protocol.Response{
		Status:   status,
		Metadata: map[string]string{},
		Body:     fmt.Sprintf("\n# %s\n\n%s\n", statusTitle(status), message),
	}
	resp.WriteTo(w)
}

func statusTitle(s string) string {
	return strings.ToUpper(s[:1]) + strings.ReplaceAll(s[1:], "-", " ")
}

func stripFrontmatter(content string) (string, map[string]string) {
	meta := make(map[string]string)

	if !strings.HasPrefix(content, "---\n") {
		return content, meta
	}

	end := strings.Index(content[4:], "\n---\n")
	if end == -1 {
		return content, meta
	}

	fmBlock := content[4 : 4+end]
	for _, line := range strings.Split(fmBlock, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if ok {
			meta[strings.TrimSpace(key)] = strings.TrimSpace(val)
		}
	}

	body := content[4+end+5:]
	return body, meta
}
