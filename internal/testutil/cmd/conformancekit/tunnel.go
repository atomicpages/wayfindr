package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"time"
)

// serveTunnel stands in for a tunnel in front of the daemon (ADR 0021): it
// serves https on listen with a throwaway self-signed certificate for the
// listen address's IP, writes that certificate to certOut for the suite to
// trust, and forwards every request to target as a tunnel does. It adds the
// X-Forwarded-* headers, which make the daemon treat the request as remote,
// and keeps the Host the client sent. It adds no Authorization header: a
// caller reaches the daemon's public routes the way an internet client would.
func serveTunnel(ctx context.Context, listen, target, certOut string) error {
	host, _, err := net.SplitHostPort(listen)

	if err != nil {
		return fmt.Errorf("-listen %q: %w", listen, err)
	}

	ip := net.ParseIP(host)

	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("-listen %q: the host must be a loopback IP address", listen)
	}

	if _, _, err := net.SplitHostPort(target); err != nil {
		return fmt.Errorf("-target %q: %w", target, err)
	}

	if certOut == "" {
		return errors.New("-cert-out is required")
	}

	cert, certPEM, err := selfSigned(ip)

	if err != nil {
		return err
	}

	if err := os.WriteFile(certOut, certPEM, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", certOut, err)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", listen)

	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	h := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = target
			pr.SetXForwarded()
		},
		FlushInterval: -1,
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})

	if err := srv.Serve(tlsLn); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	return nil
}

// selfSigned returns a certificate for ip, valid for a day, and its PEM.
func selfSigned(ip net.IP) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("key: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "conformancekit tunnel"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{ip},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)

	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("certificate: %w", err)
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
