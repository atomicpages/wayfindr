// Package testcert makes the certificates a test of a connector's TLS needs
// (design §4, [connectors.<name>.tls]): a private certificate authority made
// for the one test, and the server and client certificates it issues.
// Nothing is read from disk, and every key is a fresh ECDSA P-256 key.
package testcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CA is a private certificate authority made for one test.
type CA struct {
	Cert *x509.Certificate
	key  *ecdsa.PrivateKey
	// PEM is Cert as PEM, what a ca_file holds.
	PEM []byte
}

// NewCA makes a CA valid from an hour ago to a day from now.
func NewCA(t testing.TB) *CA {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)

	if err != nil {
		t.Fatalf("create ca: %v", err)
	}

	cert, err := x509.ParseCertificate(der)

	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}

	return &CA{Cert: cert, key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// Pool is a pool holding only the CA, for a server's ClientCAs.
func (a *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.Cert)

	return pool
}

// Issue returns a PEM certificate and key for cn, valid for the next hour:
// a server certificate for 127.0.0.1 and localhost when server is true,
// else a client certificate.
func (a *CA) Issue(t testing.TB, cn string, server bool) (certPEM, keyPEM []byte) {
	t.Helper()

	return a.IssueUntil(t, cn, server, time.Now().Add(time.Hour))
}

// IssueUntil is Issue with the certificate's NotAfter.
func (a *CA) IssueUntil(t testing.TB, cn string, server bool, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key := newKey(t)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))

	if err != nil {
		t.Fatalf("serial: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
		tmpl.DNSNames = []string{"localhost"}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.Cert, &key.PublicKey, a.key)

	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)

	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
}

// ServerCert is a server certificate for 127.0.0.1 and localhost, ready
// for a tls.Config.
func (a *CA) ServerCert(t testing.TB) tls.Certificate {
	t.Helper()
	certPEM, keyPEM := a.Issue(t, "server", true)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)

	if err != nil {
		t.Fatalf("server key pair: %v", err)
	}

	return cert
}

// Write writes data to name in dir and returns its path.
func Write(t testing.TB, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
}

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return key
}
