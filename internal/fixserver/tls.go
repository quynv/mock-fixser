package fixserver

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/quynv/mock-fixser/internal/config"
)

// tlsConfig builds the client acceptor's TLS configuration (section 5.1).
func tlsConfig(t *config.TLS) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("server.fix.tls: %w", err)
	}
	tc := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	switch t.ClientAuth {
	case "", "none":
		tc.ClientAuth = tls.NoClientCert
	case "request":
		tc.ClientAuth = tls.RequestClientCert
	case "require":
		tc.ClientAuth = tls.RequireAndVerifyClientCert
	default:
		return nil, fmt.Errorf("server.fix.tls.client_auth: unknown value %q", t.ClientAuth)
	}
	if t.ClientCAFile != "" {
		pem, err := os.ReadFile(t.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("server.fix.tls.client_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("server.fix.tls.client_ca_file: no certificates in %s", t.ClientCAFile)
		}
		tc.ClientCAs = pool
	}
	return tc, nil
}
