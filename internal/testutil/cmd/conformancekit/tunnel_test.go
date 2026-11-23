package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// The tunnel serves https with the certificate it wrote, forwards as a tunnel
// does (X-Forwarded-*, the Host the client sent), and adds no token. The
// daemon admits the public OAuth routes by that Host, so it must survive.
func TestTunnelServesHTTPSAndForwards(t *testing.T) {
	got := make(chan http.Header, 1)
	host := make(chan string, 1)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		host <- r.Host

		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(daemon.Close)

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	listen := ln.Addr().String()

	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	certOut := filepath.Join(t.TempDir(), "tunnel.pem")

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	exited := false // set once a poll loop has received serveTunnel's error

	go func() { served <- serveTunnel(ctx, listen, daemon.Listener.Addr().String(), certOut) }()

	// A tunnel that fails to start must fail the test at once, not after both
	// poll loops time out; and one that fails on the way out must not go unseen.
	t.Cleanup(func() {
		cancel()

		if exited {
			return
		}

		if err := <-served; err != nil {
			t.Errorf("serveTunnel: %v", err)
		}
	})

	var pemBytes []byte

	for range 100 {
		select {
		case err := <-served:
			exited = true

			t.Fatalf("serveTunnel exited before the certificate was written: %v", err)
		default:
		}

		if pemBytes, err = os.ReadFile(filepath.Clean(certOut)); err == nil && len(pemBytes) > 0 {
			break
		}

		time.Sleep(20 * time.Millisecond)
	}

	pool := x509.NewCertPool()

	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("no certificate in %s", certOut)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	t.Cleanup(client.CloseIdleConnections)

	var resp *http.Response

	for range 100 {
		select {
		case err := <-served:
			exited = true

			t.Fatalf("serveTunnel exited before answering: %v", err)
		default:
		}

		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+listen+"/x", nil)

		if resp, err = client.Do(req); err == nil {
			break
		}

		time.Sleep(20 * time.Millisecond)
	}

	if err != nil {
		t.Fatalf("GET through the tunnel: %v", err)
	}

	_ = resp.Body.Close()

	h := <-got

	if h.Get("X-Forwarded-For") == "" || h.Get("X-Forwarded-Proto") != "https" {
		t.Errorf("forwarding headers = %v, want X-Forwarded-For and X-Forwarded-Proto https", h)
	}

	if h.Get("X-Forwarded-Host") == "" {
		t.Errorf("forwarding headers = %v, want X-Forwarded-Host", h)
	}

	if h.Get("Authorization") != "" {
		t.Errorf("tunnel added Authorization %q", h.Get("Authorization"))
	}

	if diff := cmp.Diff(listen, <-host); diff != "" {
		t.Errorf("Host (-want +got):\n%s", diff)
	}
}
