package main

import (
	"testing"

	"github.com/latebit/demarkus/protocol"
)

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
