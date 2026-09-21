package protocol

import (
	"errors"
	"strings"
	"testing"
)

func TestSplitFrontmatter(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		found     bool
		block     string
		body      string
		closedEOF bool
		wantErr   error
	}{
		{name: "no fence", in: "# Title\n", body: "# Title\n"},
		{name: "empty input"},
		{name: "block and body", in: "---\na: b\n---\nbody\n", found: true, block: "a: b", body: "body\n"},
		{name: "block without body", in: "---\na: b\n---\n", found: true, block: "a: b"},
		{name: "close at end of input", in: "---\na: b\n---", found: true, block: "a: b", closedEOF: true},
		{name: "blank block", in: "---\n\n---\n---\nx\n", found: true, body: "---\nx\n"},
		{name: "first close wins", in: "---\na: b\n---\nbody\n---\nmore", found: true, block: "a: b", body: "body\n---\nmore"},
		{name: "unclosed", in: "---\na: b\n", wantErr: ErrUnclosedFrontmatter},
		{name: "fence without newline is body", in: "---", body: "---"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SplitFrontmatter([]byte(tt.in))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SplitFrontmatter: %v", err)
			}
			if got.Found != tt.found || got.ClosedAtEOF != tt.closedEOF {
				t.Errorf("found %v closedEOF %v, want %v %v", got.Found, got.ClosedAtEOF, tt.found, tt.closedEOF)
			}
			if string(got.Block) != tt.block {
				t.Errorf("block = %q, want %q", got.Block, tt.block)
			}
			if string(got.Body) != tt.body {
				t.Errorf("body = %q, want %q", got.Body, tt.body)
			}
		})
	}
}

// Both wire parsers accept the close at end of input form.
func TestParseResponseAcceptsCloseAtEndOfInput(t *testing.T) {
	resp, err := ParseResponse(strings.NewReader("---\nstatus: ok\n---"))
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if resp.Status != StatusOK || resp.Body != "" {
		t.Errorf("status %q body %q", resp.Status, resp.Body)
	}
}
