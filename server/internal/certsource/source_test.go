package certsource

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenValidatesAuthorityCoverageAndTime(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name        string
		dnsNames    []string
		notBefore   time.Time
		notAfter    time.Time
		authorities []string
		wantErr     string
	}{
		{
			name:        "multi SAN",
			dnsNames:    []string{"a.example.com", "b.example.com"},
			notBefore:   now.Add(-time.Hour),
			notAfter:    now.Add(time.Hour),
			authorities: []string{"a.example.com", "b.example.com"},
		},
		{
			name:        "wildcard coverage",
			dnsNames:    []string{"*.example.com"},
			notBefore:   now.Add(-time.Hour),
			notAfter:    now.Add(time.Hour),
			authorities: []string{"a.example.com"},
		},
		{
			name:        "SAN gap",
			dnsNames:    []string{"a.example.com"},
			notBefore:   now.Add(-time.Hour),
			notAfter:    now.Add(time.Hour),
			authorities: []string{"b.example.com"},
			wantErr:     "does not cover authority",
		},
		{
			name:        "not yet valid",
			dnsNames:    []string{"a.example.com"},
			notBefore:   now.Add(time.Hour),
			notAfter:    now.Add(2 * time.Hour),
			authorities: []string{"a.example.com"},
			wantErr:     "is not valid before",
		},
		{
			name:        "expired",
			dnsNames:    []string{"a.example.com"},
			notBefore:   now.Add(-2 * time.Hour),
			notAfter:    now.Add(-time.Hour),
			authorities: []string{"a.example.com"},
			wantErr:     "expired at",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			certificateFile, keyFile := writeKeyPair(t, test.dnsNames, test.notBefore, test.notAfter)
			source, err := Open(certificateFile, keyFile, test.authorities)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Open error = %v, want substring %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			certificate, err := source.GetCertificate(nil)
			if err != nil || certificate.Leaf == nil {
				t.Fatalf("GetCertificate = (%v, %v), want parsed leaf", certificate, err)
			}
		})
	}
}

func TestReloadRetainsLastKnownGoodCertificate(t *testing.T) {
	now := time.Now()
	certificateFile, keyFile := writeKeyPair(t, []string{"world.example.com"}, now.Add(-time.Hour), now.Add(time.Hour))
	source, err := Open(certificateFile, keyFile, []string{"world.example.com"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	current := source.current.Load()

	replacementCertificate, replacementKey := generateKeyPair(t, []string{"other.example.com"}, now.Add(-time.Hour), now.Add(time.Hour))
	writePEM(t, certificateFile, replacementCertificate, "CERTIFICATE")
	writePEM(t, keyFile, replacementKey, "PRIVATE KEY")
	if err := source.Reload(); err == nil {
		t.Fatal("Reload accepted certificate with SAN gap")
	}
	if source.current.Load() != current {
		t.Fatal("failed reload replaced current certificate")
	}
}

func TestTLSConfig(t *testing.T) {
	now := time.Now()
	certificateFile, keyFile := writeKeyPair(t, []string{"world.example.com"}, now.Add(-time.Hour), now.Add(time.Hour))
	source, err := Open(certificateFile, keyFile, []string{"world.example.com"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	config := source.TLSConfig("mark-test")
	if config.MinVersion != tls.VersionTLS13 || len(config.NextProtos) != 1 || config.NextProtos[0] != "mark-test" {
		t.Fatalf("TLSConfig = %+v", config)
	}
	if _, err := config.GetCertificate(nil); err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
}

func writeKeyPair(t *testing.T, dnsNames []string, notBefore, notAfter time.Time) (certificateFile, keyFile string) {
	t.Helper()
	certificate, key := generateKeyPair(t, dnsNames, notBefore, notAfter)
	directory := t.TempDir()
	certificateFile = filepath.Join(directory, "tls.crt")
	keyFile = filepath.Join(directory, "tls.key")
	writePEM(t, certificateFile, certificate, "CERTIFICATE")
	writePEM(t, keyFile, key, "PRIVATE KEY")
	return certificateFile, keyFile
}

func generateKeyPair(t *testing.T, dnsNames []string, notBefore, notAfter time.Time) (certificateDER, keyDER []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificateDER, err = x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err = x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return certificateDER, keyDER
}

func writePEM(t *testing.T, filename string, der []byte, blockType string) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
}
