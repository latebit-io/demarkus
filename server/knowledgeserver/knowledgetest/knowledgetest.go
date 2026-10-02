// Package knowledgetest opens real knowledge servers over in-memory buckets,
// for tests outside the server module.
package knowledgetest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/knowledgeserver"
)

// World is one world a server serves, routed at Authority.
type World struct {
	Name, Authority string
}

// Buckets holds in-memory buckets by name: servers opened over one Buckets
// are replicas of one deployment. The zero value holds none yet.
type Buckets struct {
	mu      sync.Mutex
	buckets map[string]*blob.Memory
}

// Open opens bucket, empty on first use.
func (b *Buckets) Open(_ context.Context, bucket string, maxObjectBytes int64) (blob.Store, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if store := b.buckets[bucket]; store != nil {
		return store, nil
	}
	store, err := blob.NewMemory(maxObjectBytes)
	if err != nil {
		return nil, err
	}
	if b.buckets == nil {
		b.buckets = map[string]*blob.Memory{}
	}
	b.buckets[bucket] = store
	return store, nil
}

// Open serves worlds over buckets until t ends: without token files, each
// is readable anonymously and writable under a grant, by the default policy.
// Nothing listens until Serve.
func Open(t testing.TB, buckets *Buckets, worlds ...World) *knowledgeserver.Server {
	t.Helper()
	dir := t.TempDir()
	var config strings.Builder
	certFile, keyFile := writeCert(t, dir, worlds)
	fmt.Fprintf(&config, "version: 1\ntls:\n  certFile: %s\n  keyFile: %s\nlisten:\n  address: 127.0.0.1:0\nhealth:\n  address: 127.0.0.1:0\nworlds:\n", certFile, keyFile)
	for _, w := range worlds {
		fmt.Fprintf(&config, "  - name: %s\n    authorities: [%s]\n    bucket:\n      url: gs://knowledgetest-%s\n      worldID: %s\n",
			w.Name, w.Authority, w.Name, worldID(w.Name))
	}
	configFile := filepath.Join(dir, "config.yaml")
	write(t, configFile, []byte(config.String()))
	server, err := knowledgeserver.Open(knowledgeserver.Options{ConfigFile: configFile, Logger: slog.New(slog.DiscardHandler), Buckets: buckets.Open})
	if err != nil {
		t.Fatalf("open knowledge server for %v: %v", worlds, err)
	}
	t.Cleanup(server.Close)
	return server
}

// worldID is a stable RFC 4122 shaped id per name, so replicas agree.
func worldID(name string) string {
	sum := sha256.Sum256([]byte(name))
	id := []byte(hex.EncodeToString(sum[:16]))
	id[12], id[16] = '4', '8'
	return fmt.Sprintf("%s-%s-%s-%s-%s", id[0:8], id[8:12], id[12:16], id[16:20], id[20:32])
}

// writeCert writes a self-signed certificate naming every world's authority.
func writeCert(t testing.TB, dir string, worlds []World) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "knowledgetest"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, w := range worlds {
		template.DNSNames = append(template.DNSNames, w.Authority)
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	write(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	write(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certFile, keyFile
}

func write(t testing.TB, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
