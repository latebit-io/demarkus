package doctor

import (
	"context"
	"fmt"
	"maps"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/render"
)

// fakeDoc is one stored document; Status defaults to ok.
type fakeDoc struct {
	Body     string
	Meta     map[string]string
	Status   string
	Versions map[int]fakeDoc // earlier versions for --deep
}

// fakeStore serves LIST pages in the server's shape (sorted names, opaque
// cursor = last name) so listing.ParsePage sees real pagination.
type fakeStore struct {
	docs       map[string]fakeDoc
	pageSize   int
	listCalls  int
	fetchCalls int
	listStatus map[string]string // dir -> status override
	missing    map[string]string // path outside docs -> fetch status
	stuck      bool              // return the first page with the same cursor forever
}

func newFake(docs map[string]fakeDoc) *fakeStore {
	return &fakeStore{docs: docs, pageSize: 2, listStatus: map[string]string{}, missing: map[string]string{}}
}

func (f *fakeStore) List(_ context.Context, dir string, _ bool, cursor string) (protocol.Response, error) {
	f.listCalls++
	if st := f.listStatus[dir]; st != "" {
		return protocol.Response{Status: st}, nil
	}
	names := map[string]bool{}
	for p := range f.docs {
		if !strings.HasPrefix(p, dir) {
			continue
		}
		rest := strings.TrimPrefix(p, dir)
		if head, _, nested := strings.Cut(rest, "/"); nested {
			names[head+"/"] = true
		} else {
			names[rest] = true
		}
	}
	if len(names) == 0 {
		return protocol.Response{Status: protocol.StatusNotFound}, nil
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Slice(sorted, func(i, j int) bool { return strings.TrimSuffix(sorted[i], "/") < strings.TrimSuffix(sorted[j], "/") })
	start := 0
	if cursor != "" && !f.stuck {
		for start < len(sorted) && strings.TrimSuffix(sorted[start], "/") <= cursor {
			start++
		}
	}
	end := min(start+f.pageSize, len(sorted))
	// The page is the server's own rendering, so the walker is tested
	// against the real shape, escaping included.
	entries := make([]render.ListEntry, 0, end-start)
	for _, n := range sorted[start:end] {
		entries = append(entries, render.ListEntry{Name: strings.TrimSuffix(n, "/"), IsDir: strings.HasSuffix(n, "/")})
	}
	nextCursor := ""
	if end < len(sorted) {
		nextCursor = strings.TrimSuffix(sorted[end-1], "/")
		if f.stuck {
			nextCursor = "x"
		}
	}
	return render.ListResponse(dir, entries, nextCursor), nil
}

func (f *fakeStore) Fetch(_ context.Context, docPath string) (protocol.Response, error) {
	f.fetchCalls++
	if base := path.Base(docPath); strings.HasPrefix(base, "v") {
		if v, err := strconv.Atoi(base[1:]); err == nil {
			d, ok := f.docs[path.Dir(docPath)]
			if !ok {
				return protocol.Response{Status: protocol.StatusNotFound}, nil
			}
			old, ok := d.Versions[v]
			if !ok {
				return protocol.Response{Status: protocol.StatusNotFound}, nil
			}
			return protocol.Response{Status: protocol.StatusOK, Metadata: old.Meta, Body: old.Body}, nil
		}
	}
	d, ok := f.docs[docPath]
	if !ok {
		if st := f.missing[docPath]; st != "" {
			return protocol.Response{Status: st}, nil
		}
		return protocol.Response{Status: protocol.StatusNotFound}, nil
	}
	status := d.Status
	if status == "" {
		status = protocol.StatusOK
	}
	meta := maps.Clone(d.Meta)
	if meta == nil {
		meta = map[string]string{}
	}
	if _, ok := meta["version"]; !ok {
		meta["version"] = strconv.Itoa(len(d.Versions) + 1)
	}
	return protocol.Response{Status: status, Metadata: meta, Body: d.Body}, nil
}

func (f *fakeStore) Versions(_ context.Context, docPath string) (protocol.Response, error) {
	d, ok := f.docs[docPath]
	if !ok {
		return protocol.Response{Status: protocol.StatusNotFound}, nil
	}
	history := make([]render.VersionEntry, 0, len(d.Versions)+1)
	for v := len(d.Versions) + 1; v >= 1; v-- {
		history = append(history, render.VersionEntry{Version: v})
	}
	return render.VersionsResponse(docPath, history, d.Meta["chain-valid"] != "false"), nil
}

func tagged(body string) fakeDoc {
	return fakeDoc{Body: body, Meta: map[string]string{"tags": "a,b", "type": "Guide"}}
}

func findings(r *Report, check string) []string {
	var out []string
	for _, f := range r.Findings {
		if f.Check == check {
			out = append(out, fmt.Sprintf("%s: %s", f.Path, f.Detail))
		}
	}
	return out
}
