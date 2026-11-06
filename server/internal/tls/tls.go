// Package tls provides TLS configuration for the Demarkus server.
package tls

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// GenerateDevConfig creates a self-signed TLS config for development.
func GenerateDevConfig() (*tls.Config, error) {
	certificate, err := GenerateDevCertificate()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{protocol.ALPN},
	}, nil
}

// GenerateDevCertificate is an ephemeral self-signed Ed25519 certificate for
// localhost and the loopback addresses, valid for a year; the certificate
// source mints another if a soul outlives it.
func GenerateDevCertificate() (tls.Certificate, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore:    now,
		NotAfter:     now.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, priv.Public(), priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: priv}, nil
}
