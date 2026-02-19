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
		{protocol.VerbWrite, false},
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
