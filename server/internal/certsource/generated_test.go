package certsource

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	servertls "github.com/latebit-io/demarkus/server/internal/tls"
)

// A generated source serves what its generator minted, mints again on a
// reload, and replaces an expired leaf on the handshake that finds it: a
// soul with no certificate pair runs for days.
func TestGeneratedMintsAgainOnReloadAndExpiry(t *testing.T) {
	clock := time.Now()
	minted := 0
	// Mint against the test's clock, one day of validity, as the dev
	// certificate does against the wall clock.
	source, err := Generated(func() (tls.Certificate, error) {
		minted++
		certificateDER, keyDER := generateKeyPair(t, []string{"localhost"}, clock, clock.Add(24*time.Hour))
		key, err := x509.ParsePKCS8PrivateKey(keyDER)
		if err != nil {
			return tls.Certificate{}, err
		}
		return tls.Certificate{Certificate: [][]byte{certificateDER}, PrivateKey: key}, nil
	})
	if err != nil {
		t.Fatalf("Generated: %v", err)
	}
	source.now = func() time.Time { return clock }
	first, err := source.GetCertificate(nil)
	if err != nil || minted != 1 {
		t.Fatalf("GetCertificate: %v, minted %d", err, minted)
	}
	if err := source.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	second, err := source.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after reload: %v", err)
	}
	if minted != 2 || second.Leaf.Equal(first.Leaf) {
		t.Errorf("reload: minted %d, same leaf %t; want a fresh certificate", minted, second.Leaf.Equal(first.Leaf))
	}

	// Two days on, the leaf has expired: the handshake mints a new one.
	clock = clock.Add(48 * time.Hour)
	third, err := source.GetCertificate(nil)
	if err != nil || minted != 3 {
		t.Fatalf("expired leaf on a generated source: %v, minted %d; want a third certificate", err, minted)
	}
	if third.Leaf.NotAfter.Before(clock) {
		t.Error("the replacement certificate is already expired")
	}
}

// The dev certificate is what the generated source serves for the local
// profile and covers the names a local client dials.
func TestDevCertificateCoversLocalhost(t *testing.T) {
	source, err := Generated(servertls.GenerateDevCertificate)
	if err != nil {
		t.Fatalf("Generated: %v", err)
	}
	if err := source.Covers("localhost"); err != nil {
		t.Errorf("the dev certificate does not cover localhost: %v", err)
	}
}

// A file-backed source still refuses an expired certificate: a reload of
// the same files cannot repair it.
func TestFileSourceRefusesAnExpiredCertificate(t *testing.T) {
	now := time.Now()
	certificateFile, keyFile := writeKeyPair(t, []string{"world.example.com"}, now.Add(-2*time.Hour), now.Add(-time.Hour))
	if _, err := Open(certificateFile, keyFile, nil); err == nil {
		t.Fatal("Open accepted an expired certificate")
	}
}
