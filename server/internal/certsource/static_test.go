package certsource

import (
	"testing"

	servertls "github.com/latebit-io/demarkus/server/internal/tls"
)

// A static source serves the in-memory certificate it was given and a
// reload re-validates the same certificate: there is no file to read.
func TestStaticServesAndReloadsAsANoOp(t *testing.T) {
	dev, err := servertls.GenerateDevCertificate()
	if err != nil {
		t.Fatal(err)
	}
	source, err := Static(dev)
	if err != nil {
		t.Fatalf("Static: %v", err)
	}
	before, err := source.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if err := source.Reload(); err != nil {
		t.Fatalf("Reload on a static source: %v", err)
	}
	if after, _ := source.GetCertificate(nil); !after.Leaf.Equal(before.Leaf) {
		t.Error("reload replaced the static certificate")
	}
	if err := source.Covers("localhost"); err != nil {
		t.Errorf("the dev certificate does not cover localhost: %v", err)
	}
}
