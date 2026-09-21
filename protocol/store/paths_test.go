package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocate(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	tests := []struct {
		name    string
		reqPath string
		rel     string
		docDir  string
		reqV2   string
		wantErr error
	}{
		{name: "root document", reqPath: "/doc.md", rel: "doc.md", docDir: "versions/doc.md", reqV2: "/versions/doc.md/v2"},
		{name: "nested", reqPath: "/a/b/doc.md", rel: "a/b/doc.md", docDir: "a/b/versions/doc.md", reqV2: "/a/b/versions/doc.md/v2"},
		{name: "unclean spelling", reqPath: "a//b/./doc.md", rel: "a/b/doc.md", docDir: "a/b/versions/doc.md", reqV2: "/a/b/versions/doc.md/v2"},
		{name: "traversal", reqPath: "/a/../doc.md", wantErr: os.ErrNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc, err := s.locate(tt.reqPath)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("locate: %v", err)
			}
			if loc.rel != tt.rel {
				t.Errorf("rel = %q, want %q", loc.rel, tt.rel)
			}
			if want := filepath.Join(root, filepath.FromSlash(tt.docDir)); loc.docDir != want {
				t.Errorf("docDir = %q, want %q", loc.docDir, want)
			}
			if want := filepath.Join(root, filepath.FromSlash(tt.rel)); loc.current != want {
				t.Errorf("current = %q, want %q", loc.current, want)
			}
			if got := loc.versionFile(2); got != filepath.Join(loc.docDir, "v2") {
				t.Errorf("versionFile = %q", got)
			}
			if got := loc.versionReqPath(2); got != tt.reqV2 {
				t.Errorf("versionReqPath = %q, want %q", got, tt.reqV2)
			}
		})
	}
}
