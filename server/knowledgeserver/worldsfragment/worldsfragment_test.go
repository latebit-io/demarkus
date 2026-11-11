package worldsfragment

import (
	"bytes"
	"strings"
	"testing"
)

func tenant(name string) World {
	w := World{Name: name, Authorities: []string{name + ".tenants.example.com"}, Gated: true}
	w.Bucket = Bucket{URL: "gs://memory-" + name, WorldID: "0b8f6d0e-4c9d-4a55-8f43-2f3f6c1f9a01"}
	w.Limits = &Limits{MaxDocuments: 500}
	w.Policy = &Policy{Governance: "open"}
	return w
}

func TestRenderParseRoundTrip(t *testing.T) {
	data, err := Render([]World{tenant("zed-1"), tenant("amy-2")})
	if err != nil {
		t.Fatal(err)
	}
	if !Generated(data) {
		t.Fatalf("rendered fragment lacks the header:\n%s", data)
	}
	doc, unknown, err := Parse(data)
	if err != nil || unknown != "" {
		t.Fatalf("Parse: unknown %q, err %v", unknown, err)
	}
	if len(doc.Worlds) != 2 || doc.Worlds[0].Name != "amy-2" || doc.Worlds[1].Name != "zed-1" {
		t.Fatalf("worlds = %+v, want both, by name", doc.Worlds)
	}
	if got := doc.Worlds[0]; !got.Gated || got.Limits.MaxDocuments != 500 || got.Policy.Governance != "open" || got.Bucket.URL != "gs://memory-amy-2" {
		t.Errorf("world did not survive the round trip: %+v", got)
	}
}

func TestRenderIsStableAndEmptyIsADocument(t *testing.T) {
	a, _ := Render([]World{tenant("b"), tenant("a")})
	b, _ := Render([]World{tenant("a"), tenant("b")})
	if !bytes.Equal(a, b) {
		t.Errorf("render depends on input order:\n%s\n%s", a, b)
	}
	empty, err := Render(nil)
	if err != nil || !Generated(empty) || !strings.Contains(string(empty), "worlds: []") {
		t.Errorf("empty render = %q, %v", empty, err)
	}
	if doc, unknown, err := Parse(empty); err != nil || unknown != "" || len(doc.Worlds) != 0 {
		t.Errorf("empty parse = %+v, %q, %v", doc, unknown, err)
	}
}

// A newer provisioner's key must not stall a server of this build: the
// worlds still parse, and the key is named for the log.
func TestParseSkipsAndReportsUnknownKeys(t *testing.T) {
	data := Header + `worlds:
  - name: eve-1
    authorities: [eve-1.tenants.example.com]
    bucket: {url: "gs://memory-eve-1", worldID: w1, region: eu}
    gated: true
    futureKnob: 3
`
	doc, unknown, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(doc.Worlds) != 1 || !doc.Worlds[0].Gated || doc.Worlds[0].Bucket.URL != "gs://memory-eve-1" {
		t.Fatalf("known fields lost: %+v", doc.Worlds)
	}
	for _, key := range []string{"futureKnob", "region"} {
		if !strings.Contains(unknown, key) {
			t.Errorf("unknown = %q, want it to name %s", unknown, key)
		}
	}
}

func TestParseRefusesMalformedAndMultipleDocuments(t *testing.T) {
	for name, data := range map[string]string{
		"malformed":  Header + "worlds: [",
		"two docs":   Header + "worlds: []\n---\nworlds: []\n",
		"wrong type": Header + "worlds:\n  - name: [a]\n",
	} {
		if _, _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestGeneratedTellsAnOperatorsFile(t *testing.T) {
	if Generated([]byte("worlds: []\n")) || Generated(nil) {
		t.Error("a file without the header reads as generated")
	}
}
