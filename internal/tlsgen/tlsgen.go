// Package tlsgen writes self-signed certificates for testing mockfix over
// TLS (server.fix.tls).
package tlsgen

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Generate writes ca.crt, server.crt/key and client.crt/key to dir. The
// server certificate covers hosts (DNS names or IPs).
func Generate(dir string, hosts []string, validity time.Duration) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	caKey, caCert, caDER, err := newCert(nil, nil, &x509.Certificate{
		Subject:               pkix.Name{CommonName: "mockfix test CA"},
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}, validity)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, "ca.crt"), "CERTIFICATE", caDER); err != nil {
		return err
	}
	server := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "mockfix"},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(strings.TrimSpace(h)); ip != nil {
			server.IPAddresses = append(server.IPAddresses, ip)
		} else if h = strings.TrimSpace(h); h != "" {
			server.DNSNames = append(server.DNSNames, h)
		}
	}
	client := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "mockfix client"},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	for name, tmpl := range map[string]*x509.Certificate{"server": server, "client": client} {
		key, _, der, err := newCert(caKey, caCert, tmpl, validity)
		if err != nil {
			return err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		if err := writePEM(filepath.Join(dir, name+".crt"), "CERTIFICATE", der); err != nil {
			return err
		}
		if err := writePEM(filepath.Join(dir, name+".key"), "PRIVATE KEY", keyDER); err != nil {
			return err
		}
	}
	return nil
}

// newCert signs tmpl with the parent key (self-signed when parent is nil).
func newCert(parentKey *ecdsa.PrivateKey, parent *x509.Certificate, tmpl *x509.Certificate, validity time.Duration) (*ecdsa.PrivateKey, *x509.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl.SerialNumber = serial
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().Add(validity)
	signer, signerCert := key, tmpl
	if parentKey != nil {
		signer, signerCert = parentKey, parent
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signerCert, &key.PublicKey, signer)
	if err != nil {
		return nil, nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return key, cert, der, err
}

func writePEM(path, typ string, der []byte) error {
	mode := os.FileMode(0o644)
	if typ == "PRIVATE KEY" {
		mode = 0o600
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}
