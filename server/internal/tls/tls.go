// Package tls provides TLS configuration for the Demarkus server.
package tls

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"

	"github.com/latebit/demarkus/protocol"
)

// GenerateDevConfig creates a self-signed TLS config for development.
// It generates an ephemeral Ed25519 certificate in memory.
func GenerateDevConfig() (*tls.Config, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"localhost"},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, priv.Public(), priv)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{certDER},
			PrivateKey:  priv,
		}},
		NextProtos: []string{protocol.ALPN},
	}, nil
}
