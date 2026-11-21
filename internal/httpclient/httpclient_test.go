package httpclient_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/httpclient"
	"github.com/atomicpages/wayfindr/internal/testutil/testcert"
)

func newCA(t *testing.T) *testcert.CA { return testcert.NewCA(t) }

// newServer starts a TLS server with a certificate from a, which answers
// with the common name of the client certificate it got, or "anonymous".
// With clientCerts it requires a client certificate issued by a.
func newServer(t *testing.T, a *testcert.CA, clientCerts bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 {
			_, _ = io.WriteString(w, "anonymous")

			return
		}

		_, _ = io.WriteString(w, r.TLS.PeerCertificates[0].Subject.CommonName) //nolint:gosec // G705: a test server's plain-text echo of a test certificate
	}))
	certPEM, keyPEM := a.Issue(t, "server", true)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)

	if err != nil {
		t.Fatalf("server key pair: %v", err)
	}

	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}

	if clientCerts {
		srv.TLS.ClientAuth = tls.RequireAndVerifyClientCert
		srv.TLS.ClientCAs = a.Pool()
	}

	// The rejected handshakes are expected; keep them out of the test log.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func get(t *testing.T, c *http.Client, url string) (string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)

	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := c.Do(req)

	if err != nil {
		return "", err
	}

	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)

	return string(body), err
}

// secrets resolves ${secret:…} from m; any other reference is not found.
func secrets(m map[string]string) config.Resolver {
	return config.ResolverFunc(func(_ context.Context, ref config.Ref) (string, error) {
		if v, ok := m[ref.Name]; ok && ref.Kind == config.RefSecret {
			return v, nil
		}

		return "", fmt.Errorf("%s: %w", ref, config.ErrNotFound)
	})
}

func connector(t *config.TLS) config.Connector {
	return config.Connector{Name: "corp", URL: "https://corp.example.com/mcp", TLS: t}
}

// TestForPresentsTheClientCertificate: a server that requires a client
// certificate from a private CA accepts the one the tls table names, with
// its key from key_file or from a ${secret:…} key.
func TestForPresentsTheClientCertificate(t *testing.T) {
	a := newCA(t)
	srv := newServer(t, a, true)
	dir := t.TempDir()
	certPEM, keyPEM := a.Issue(t, "client-1", false)
	certFile, keyFile, caFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "c.key"), filepath.Join(dir, "ca.PEM")
	write(t, certFile, certPEM)
	write(t, keyFile, keyPEM)
	write(t, caFile, a.PEM)

	tests := []struct {
		name string
		tls  config.TLS
	}{
		{"key_file", config.TLS{CertFile: certFile, KeyFile: keyFile, CAFile: caFile}},
		{"key from a secret", config.TLS{CertFile: certFile, Key: "${secret:corp_key}", CAFile: caFile}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := httpclient.For(t.Context(), connector(&tt.tls), secrets(map[string]string{"corp_key": string(keyPEM)}))

			if err != nil {
				t.Fatalf("For: %v", err)
			}

			got, err := get(t, c, srv.URL)

			if err != nil {
				t.Fatalf("GET: %v", err)
			}

			if got != "client-1" {
				t.Errorf("server saw client certificate %q, want %q", got, "client-1")
			}
		})
	}
}

// TestForTrustsCAFile: ca_file adds a private CA to the system roots; the
// system roots alone reject a server certificate it issued.
func TestForTrustsCAFile(t *testing.T) {
	a := newCA(t)
	srv := newServer(t, a, false)
	caFile := filepath.Join(t.TempDir(), "ca.PEM")
	write(t, caFile, a.PEM)

	t.Run("ca_file", func(t *testing.T) {
		c, err := httpclient.For(t.Context(), connector(&config.TLS{CAFile: caFile}), secrets(nil))

		if err != nil {
			t.Fatalf("For: %v", err)
		}

		got, err := get(t, c, srv.URL)

		if err != nil {
			t.Fatalf("GET: %v", err)
		}

		if got != "anonymous" {
			t.Errorf("body = %q, want %q", got, "anonymous")
		}
	})

	t.Run("system roots", func(t *testing.T) {
		c, err := httpclient.For(t.Context(), connector(nil), secrets(nil))

		if err != nil {
			t.Fatalf("For: %v", err)
		}

		_, err = get(t, c, srv.URL)
		var verr *tls.CertificateVerificationError

		if !errors.As(err, &verr) {
			t.Errorf("GET error = %v, want a *tls.CertificateVerificationError", err)
		}
	})
}

// TestForPicksUpARotatedCertificate covers design scenario 3: a certificate
// rotated on disk is presented from the next handshake, without a new
// client.
func TestForPicksUpARotatedCertificate(t *testing.T) {
	a := newCA(t)
	srv := newServer(t, a, true)
	dir := t.TempDir()
	certFile, keyFile, caFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "c.key"), filepath.Join(dir, "ca.PEM")
	write(t, caFile, a.PEM)

	certPEM, keyPEM := a.Issue(t, "client-1", false)
	write(t, certFile, certPEM)
	write(t, keyFile, keyPEM)

	c, err := httpclient.For(t.Context(), connector(&config.TLS{CertFile: certFile, KeyFile: keyFile, CAFile: caFile}), secrets(nil))

	if err != nil {
		t.Fatalf("For: %v", err)
	}

	var got []string

	for i, cn := range []string{"client-1", "client-2"} {
		if i > 0 {
			certPEM, keyPEM = a.Issue(t, cn, false)
			write(t, certFile, certPEM)
			write(t, keyFile, keyPEM)

			// A coarse filesystem clock could leave the mtime unchanged.
			later := time.Now().Add(time.Duration(i) * time.Hour)

			for _, f := range []string{certFile, keyFile} {
				if err := os.Chtimes(f, later, later); err != nil {
					t.Fatalf("chtimes: %v", err)
				}
			}

			// A new connection, so a new handshake.
			c.CloseIdleConnections()
		}

		body, err := get(t, c, srv.URL)

		if err != nil {
			t.Fatalf("GET %d: %v", i, err)
		}

		got = append(got, body)
	}

	if diff := cmp.Diff([]string{"client-1", "client-2"}, got); diff != "" {
		t.Errorf("client certificates the server saw (-want +got):\n%s", diff)
	}
}

// TestForMissingSecret: a key that refers to a missing secret is For's
// error, wrapping config.ErrNotFound and naming the reference.
func TestForMissingSecret(t *testing.T) {
	a := newCA(t)
	certPEM, _ := a.Issue(t, "client-1", false)
	certFile := filepath.Join(t.TempDir(), "c.pem")
	write(t, certFile, certPEM)

	_, err := httpclient.For(t.Context(), connector(&config.TLS{CertFile: certFile, Key: "${secret:corp_key}"}), secrets(nil))

	if !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("For error = %v, want one wrapping config.ErrNotFound", err)
	}

	var rerr *config.ResolveError

	if !errors.As(err, &rerr) || rerr.Ref != (config.Ref{Kind: config.RefSecret, Name: "corp_key"}) {
		t.Errorf("For error = %v, want a *config.ResolveError for ${secret:corp_key}", err)
	}
}

// TestForRefusesBadFiles: what the files hold is checked when For builds
// the client, not at the first handshake.
func TestForRefusesBadFiles(t *testing.T) {
	a := newCA(t)
	dir := t.TempDir()
	cert1, key1 := a.Issue(t, "client-1", false)
	_, key2 := a.Issue(t, "client-2", false)
	files := map[string][]byte{"cert1": cert1, "key1": key1, "key2": key2, "junk": []byte("not PEM")}

	for name, data := range files {
		write(t, filepath.Join(dir, name), data)
	}

	f := func(name string) string { return filepath.Join(dir, name) }

	tests := []struct {
		name string
		tls  config.TLS
		want error
	}{
		{"certificate and key don't match", config.TLS{CertFile: f("cert1"), KeyFile: f("key2")}, httpclient.ErrClientCert},
		{"certificate isn't PEM", config.TLS{CertFile: f("junk"), KeyFile: f("key1")}, httpclient.ErrClientCert},
		{"key file isn't PEM", config.TLS{CertFile: f("cert1"), KeyFile: f("junk")}, httpclient.ErrClientCert},
		{"key from a secret isn't PEM", config.TLS{CertFile: f("cert1"), Key: "${secret:k}"}, httpclient.ErrClientCert},
		{"ca_file isn't PEM", config.TLS{CAFile: f("junk")}, httpclient.ErrCAFile},
		{"cert_file is missing", config.TLS{CertFile: f("absent"), KeyFile: f("key1")}, httpclient.ErrClientCert},
		{"ca_file is missing", config.TLS{CAFile: f("absent")}, httpclient.ErrCAFile},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := httpclient.For(t.Context(), connector(&tt.tls), secrets(map[string]string{"k": "not PEM"}))

			if !errors.Is(err, tt.want) {
				t.Errorf("For = %v, %v; want an error wrapping %v", c, err, tt.want)
			}
		})
	}
}

// TestClientsRetain: a reload's Retain drops the clients of connectors the
// config no longer names, so a connector added again under the same name
// gets a new client, and keeps the others.
func TestClientsRetain(t *testing.T) {
	a := newCA(t)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	write(t, caFile, a.PEM)
	cs := httpclient.NewClients(secrets(nil))
	kept, dropped := connector(&config.TLS{CAFile: caFile}), connector(&config.TLS{CAFile: caFile})
	kept.Name, dropped.Name = "kept", "dropped"

	clientFor := func(c config.Connector) *http.Client {
		t.Helper()
		client, err := cs.For(t.Context(), c)

		if err != nil {
			t.Fatalf("For %s: %v", c.Name, err)
		}

		return client
	}

	keptClient, droppedClient := clientFor(kept), clientFor(dropped)
	cs.Retain(config.Connectors{"kept": kept})

	if clientFor(kept) != keptClient {
		t.Error("Retain dropped a connector it was given, want its client kept")
	}

	if clientFor(dropped) == droppedClient {
		t.Error("a connector Retain dropped got its old client back, want a new one")
	}
}

// TestCertificateExpiry: the client certificate's NotAfter, zero for a
// connector without one.
func TestCertificateExpiry(t *testing.T) {
	a := newCA(t)
	dir := t.TempDir()
	notAfter := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	certPEM, keyPEM := a.IssueUntil(t, "client-1", false, notAfter)
	certFile := testcert.Write(t, dir, "c.pem", certPEM)
	keyFile := testcert.Write(t, dir, "c.key", keyPEM)

	got, err := httpclient.CertificateExpiry(connector(&config.TLS{CertFile: certFile, KeyFile: keyFile}))

	if err != nil || !got.Equal(notAfter) {
		t.Errorf("CertificateExpiry = %v, %v; want %v", got, err, notAfter)
	}

	for _, table := range []*config.TLS{nil, {CAFile: certFile}} {
		if got, err := httpclient.CertificateExpiry(connector(table)); err != nil || !got.IsZero() {
			t.Errorf("CertificateExpiry without a client certificate = %v, %v; want zero", got, err)
		}
	}

	_, err = httpclient.CertificateExpiry(connector(&config.TLS{CertFile: filepath.Join(dir, "absent"), KeyFile: keyFile}))

	if !errors.Is(err, httpclient.ErrClientCert) {
		t.Errorf("CertificateExpiry of a missing cert_file = %v, want an error wrapping ErrClientCert", err)
	}
}

// TestForWithoutTLS: a connector without a tls table gets a working client
// with no timeout of its own.
func TestForWithoutTLS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	c, err := httpclient.For(t.Context(), connector(nil), secrets(nil))

	if err != nil {
		t.Fatalf("For: %v", err)
	}

	if c.Timeout != 0 {
		t.Errorf("Timeout = %v, want none", c.Timeout)
	}

	got, err := get(t, c, srv.URL)

	if err != nil || got != "ok" {
		t.Errorf("GET = %q, %v; want %q", got, err, "ok")
	}
}

// TestClients: one client per connector, reused until its tls table or its
// resolved key changes; connectors without a tls table share one.
func TestClients(t *testing.T) {
	a := newCA(t)
	dir := t.TempDir()
	certPEM, keyPEM := a.Issue(t, "client-1", false)
	_, otherKey := a.Issue(t, "client-2", false)
	certFile, caFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "ca.PEM")
	write(t, certFile, certPEM)
	write(t, caFile, a.PEM)

	vals := map[string]string{"corp_key": string(keyPEM)}
	cs := httpclient.NewClients(secrets(vals))
	withKey := &config.TLS{CertFile: certFile, Key: "${secret:corp_key}"}

	clientFor := func(c config.Connector) *http.Client {
		t.Helper()
		client, err := cs.For(t.Context(), c)

		if err != nil {
			t.Fatalf("For %s: %v", c.Name, err)
		}

		return client
	}

	first := clientFor(connector(withKey))

	if again := clientFor(connector(&config.TLS{CertFile: certFile, Key: "${secret:corp_key}"})); again != first {
		t.Error("an equal tls table built a new client, want the cached one")
	}

	other := connector(withKey)
	other.Name = "other"

	if clientFor(other) == first {
		t.Error("another connector got the same client, want one per connector")
	}

	if clientFor(connector(&config.TLS{CertFile: certFile, Key: "${secret:corp_key}", CAFile: caFile})) == first {
		t.Error("a changed tls table reused the cached client, want a new one")
	}

	// The certificate no longer matches, so the cached client must not be
	// returned: a new secret value means a rebuild, and this one fails.
	vals["corp_key"] = string(otherKey)

	if _, err := cs.For(t.Context(), connector(withKey)); err == nil {
		t.Error("For after the key secret changed to a mismatched key = nil error, want a rebuild that fails")
	}

	delete(vals, "corp_key")

	if _, err := cs.For(t.Context(), connector(withKey)); !errors.Is(err, config.ErrNotFound) {
		t.Errorf("For with the key secret gone = %v, want an error wrapping config.ErrNotFound", err)
	}

	plain1, plain2 := connector(nil), connector(nil)
	plain2.Name = "plain2"

	if clientFor(plain1) != clientFor(plain2) {
		t.Error("connectors without a tls table got different clients, want one shared client")
	}
}
