package main

import (
	"strings"
	"testing"
)

func TestAllowedPointsDown(t *testing.T) {
	for layer, below := range allowed {
		for _, to := range below {
			if _, ok := allowed[to]; !ok {
				t.Errorf("%s may import unknown layer %q", layer, to)
			}
		}
	}
	// A cycle would let two layers import each other.
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(layer, trail string)
	visit = func(layer, trail string) {
		switch state[layer] {
		case 1:
			t.Fatalf("layer cycle: %s -> %s", trail, layer)
		case 2:
			return
		}
		state[layer] = 1
		for _, to := range allowed[layer] {
			visit(to, trail+" -> "+layer)
		}
		state[layer] = 2
	}
	for layer := range allowed {
		visit(layer, "")
	}
}

func TestBoundaries(t *testing.T) {
	forbidden := []struct{ from, to, why string }{
		{"store", "storeclient", "the server never imports the client (ADR 0026)"},
		{"storeclient", "store", "the client runs without the server"},
		{"model", "store", "the model sits on the store client"},
		{"soul", "knowledge", "tiers are siblings"},
		{"knowledge", "soul", "tiers are siblings"},
		{"model", "surface", "surfaces stay thin over the model"},
	}
	for _, f := range forbidden {
		if mayImport(f.from, f.to) {
			t.Errorf("%s may import %s: %s", f.from, f.to, f.why)
		}
	}
}

func TestParseMap(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int
		wantErr string
	}{
		{name: "entries, comments and blanks", in: "# head\n\na/... model # why\nb store\n", want: 2},
		{name: "unknown layer", in: "a nonsense\n", wantErr: `line 1: unknown layer "nonsense"`},
		{name: "missing layer", in: "a\n", wantErr: "line 1: want <package> <layer>"},
		{name: "extra field", in: "a model store\n", wantErr: "line 1: want <package> <layer>"},
		{name: "duplicate", in: "a model\n\na store\n", wantErr: "line 3: a already mapped on line 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := parseMap(strings.NewReader(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(m) != tt.want {
				t.Fatalf("got %d entries, want %d", len(m), tt.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	m := layerMap{
		{pattern: "client/docwrite/...", layer: "storeclient"},
		{pattern: "client/docwrite/gate", layer: "model"},
		{pattern: "server/...", layer: "store"},
		{pattern: "server/internal/...", layer: "knowledge"},
		{pattern: "tools", layer: "tool"},
	}
	tests := []struct {
		pkg  string
		want string // "" for unmapped
	}{
		{pkg: "client/docwrite", want: "storeclient"},
		{pkg: "client/docwrite/gate", want: "model"},
		{pkg: "client/docwrite/gate/sub", want: "storeclient"},
		{pkg: "client/docwritex", want: ""},
		{pkg: "server/internal/x", want: "knowledge"},
		{pkg: "server/blob", want: "store"},
		{pkg: "tools", want: "tool"},
		{pkg: "tools/x", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.pkg, func(t *testing.T) {
			e, ok := m.resolve(tt.pkg)
			if got := e.layer; got != tt.want || ok != (tt.want != "") {
				t.Fatalf("resolve(%s) = %q, %v; want %q", tt.pkg, got, ok, tt.want)
			}
		})
	}
}
