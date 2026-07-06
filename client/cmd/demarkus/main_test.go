package main

import (
	"os"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func TestMetaFlag(t *testing.T) {
	m := metaFlag{}
	if err := m.Set("tags=go,auth"); err != nil {
		t.Fatalf("Set tags: %v", err)
	}
	if err := m.Set("importance=0.9"); err != nil {
		t.Fatalf("Set importance: %v", err)
	}
	if m["tags"] != "go,auth" || m["importance"] != "0.9" {
		t.Errorf("collected = %v, want tags=go,auth importance=0.9", map[string]string(m))
	}
	if err := m.Set("noequals"); err == nil {
		t.Error("expected error for missing '='")
	}
	if err := m.Set("Bad Key=v"); err == nil {
		t.Error("expected error for invalid key characters")
	}
	if err := m.Set("=novalue"); err == nil {
		t.Error("expected error for empty key")
	}
	if metaMap(metaFlag{}) != nil {
		t.Error("metaMap of empty flag should be nil")
	}
	if got := metaMap(m); got["tags"] != "go,auth" {
		t.Errorf("metaMap dropped data: %v", got)
	}
}

func TestValidateVerb(t *testing.T) {
	tests := []struct {
		verb    string
		wantErr bool
	}{
		{protocol.VerbFetch, false},
		{protocol.VerbList, false},
		{protocol.VerbVersions, false},
		{protocol.VerbPublish, false},
		{"DELETE", true},
		{"", true},
		{"fetch", true},
	}

	for _, tt := range tests {
		t.Run("verb="+tt.verb, func(t *testing.T) {
			err := validateVerb(tt.verb)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateVerb(%q): got err=%v, wantErr=%v", tt.verb, err, tt.wantErr)
			}
		})
	}
}

func TestEditorCommand(t *testing.T) {
	tests := []struct {
		name     string
		fields   []string
		file     string
		wantName string
		wantArgs []string
	}{
		{
			name:     "simple editor",
			fields:   []string{"vi"},
			file:     "/tmp/doc.md",
			wantName: "vi",
			wantArgs: []string{"/tmp/doc.md"},
		},
		{
			name:     "editor with one arg",
			fields:   []string{"code", "-w"},
			file:     "/tmp/doc.md",
			wantName: "code",
			wantArgs: []string{"-w", "/tmp/doc.md"},
		},
		{
			name:     "editor with multiple args",
			fields:   []string{"nvim", "--cmd", "set ft=markdown"},
			file:     "/tmp/doc.md",
			wantName: "nvim",
			wantArgs: []string{"--cmd", "set ft=markdown", "/tmp/doc.md"},
		},
		{
			name:     "gui editor gets --wait injected",
			fields:   []string{"zed"},
			file:     "/tmp/doc.md",
			wantName: "zed",
			wantArgs: []string{"--wait", "/tmp/doc.md"},
		},
		{
			name:     "gui editor already has --wait",
			fields:   []string{"zed", "--wait"},
			file:     "/tmp/doc.md",
			wantName: "zed",
			wantArgs: []string{"--wait", "/tmp/doc.md"},
		},
		{
			name:     "non-gui editor with one arg",
			fields:   []string{"nano", "-R"},
			file:     "/tmp/doc.md",
			wantName: "nano",
			wantArgs: []string{"-R", "/tmp/doc.md"},
		},
		{
			name:     "full path gui editor gets --wait",
			fields:   []string{"/usr/bin/zed"},
			file:     "/tmp/doc.md",
			wantName: "/usr/bin/zed",
			wantArgs: []string{"--wait", "/tmp/doc.md"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, args := editorCommand(tt.fields, tt.file)
			if name != tt.wantName {
				t.Errorf("name: got %q, want %q", name, tt.wantName)
			}
			if len(args) != len(tt.wantArgs) {
				t.Fatalf("args length: got %d, want %d", len(args), len(tt.wantArgs))
			}
			for i := range args {
				if args[i] != tt.wantArgs[i] {
					t.Errorf("args[%d]: got %q, want %q", i, args[i], tt.wantArgs[i])
				}
			}
		})
	}
}

func TestConfirmRetention(t *testing.T) {
	// A regular file stands in for non-TTY stdin (pipes and redirects).
	nonTTY, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := nonTTY.Close(); err != nil {
			t.Errorf("close temp stdin: %v", err)
		}
	}()

	tests := []struct {
		name    string
		meta    map[string]string
		yes     bool
		wantErr bool
	}{
		{"no retention key", map[string]string{"tags": "a,b"}, false, false},
		{"nil meta", nil, false, false},
		{"retention with -yes", map[string]string{"retention": "5"}, true, false},
		{"retention non-interactive without -yes", map[string]string{"retention": "5"}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			err := confirmRetention(tt.meta, tt.yes, nonTTY, &out)
			if (err != nil) != tt.wantErr {
				t.Errorf("confirmRetention() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
