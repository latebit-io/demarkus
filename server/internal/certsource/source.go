// Package certsource atomically reloads validated TLS certificates.
package certsource

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// Source publishes certificates valid for a fixed authority set.
type Source struct {
	certFile    string
	keyFile     string
	authorities []string
	current     atomic.Pointer[tls.Certificate]
	now         func() time.Time
}

// Open loads and validates the initial certificate. An empty authority
// set skips hostname pinning: dynamic deployments add worlds at runtime
// and check coverage per world via Covers instead.
func Open(certFile, keyFile string, authorities []string) (*Source, error) {
	if certFile == "" || keyFile == "" {
		return nil, errors.New("certificate and key files are required")
	}
	source := &Source{
		certFile:    certFile,
		keyFile:     keyFile,
		authorities: append([]string(nil), authorities...),
		now:         time.Now,
	}
	if err := source.Reload(); err != nil {
		return nil, err
	}
	return source, nil
}

// Reload validates a complete replacement before publishing it.
func (source *Source) Reload() error {
	certificate, err := tls.LoadX509KeyPair(source.certFile, source.keyFile)
	if err != nil {
		return fmt.Errorf("load TLS key pair: %w", err)
	}
	if len(certificate.Certificate) == 0 {
		return errors.New("TLS certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse TLS leaf certificate: %w", err)
	}
	if err := validateTime(leaf, source.currentTime()); err != nil {
		return err
	}
	for _, authority := range source.authorities {
		if err := leaf.VerifyHostname(authority); err != nil {
			return fmt.Errorf("TLS certificate does not cover authority %q: %w", authority, err)
		}
	}
	certificate.Leaf = leaf
	source.current.Store(&certificate)
	return nil
}

// Covers reports whether the current certificate is valid for authority;
// the hot-reload path warns on uncovered dynamic worlds (routing works,
// verifying clients fail until the cert rotates).
func (source *Source) Covers(authority string) error {
	certificate := source.current.Load()
	if certificate == nil || certificate.Leaf == nil {
		return errors.New("no TLS certificate loaded")
	}
	return certificate.Leaf.VerifyHostname(authority)
}

// GetCertificate returns the current last-known-good certificate.
func (source *Source) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	certificate := source.current.Load()
	if certificate == nil {
		return nil, errors.New("no TLS certificate loaded")
	}
	if certificate.Leaf != nil {
		if err := validateTime(certificate.Leaf, source.currentTime()); err != nil {
			return nil, err
		}
	}
	return certificate, nil
}

func (source *Source) currentTime() time.Time {
	if source.now != nil {
		return source.now()
	}
	return time.Now()
}

func validateTime(certificate *x509.Certificate, now time.Time) error {
	if now.Before(certificate.NotBefore) {
		return fmt.Errorf("TLS certificate is not valid before %s", certificate.NotBefore.Format(time.RFC3339))
	}
	if !now.Before(certificate.NotAfter) {
		return fmt.Errorf("TLS certificate expired at %s", certificate.NotAfter.Format(time.RFC3339))
	}
	return nil
}

// TLSConfig returns a TLS 1.3 Mark Protocol configuration.
func (source *Source) TLSConfig(nextProtocol string) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		NextProtos:     []string{nextProtocol},
		GetCertificate: source.GetCertificate,
	}
}
