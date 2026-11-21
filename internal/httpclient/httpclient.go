// Package httpclient builds the HTTP client a url connector's traffic goes
// through: its MCP server's and its authorization server's (design §2,
// §4). A connector without a [connectors.<name>.tls] table gets the
// standard library's defaults. One with a tls table also trusts the
// table's CA file, and presents its client certificate to any server that
// asks for one.
//
// It is a leaf: it imports only the standard library and config.
package httpclient

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/atomicpages/wayfindr/internal/config"
)

// What a file of the tls table failed with. For's errors about a file wrap
// one of them, so a caller tells a trust problem from a client certificate
// problem without reading error text; an unresolvable key is a
// *config.ResolveError instead.
var (
	// ErrCAFile: ca_file cannot be read or holds no PEM certificate.
	ErrCAFile = errors.New("tls ca_file")
	// ErrClientCert: cert_file or the key cannot be read, is not PEM, or
	// the two don't match.
	ErrClientCert = errors.New("tls client certificate")
)

// For builds an HTTP client for connector c.
//
// Without a tls table the client is the standard library's default
// transport, cloned. With one, the client trusts the system roots plus
// ca_file's certificates, and presents cert_file's certificate. cert_file
// and key_file are read again at the next handshake after either changes
// on disk, so a rotated certificate is used without a restart; ca_file is
// read only here, when a client is built: a changed ca_file is trusted
// once the table changes or the daemon restarts.
//
// A key template is resolved here, so a missing ${secret:…} is For's error
// (a *config.ResolveError wrapping config.ErrNotFound) rather than a failed
// handshake later. So are an unreadable file, PEM that does not parse, and a
// certificate that does not match its key.
//
// The client has no Timeout: callers bound each request with its context.
func For(ctx context.Context, c config.Connector, r config.Resolver) (*http.Client, error) {
	if c.TLS == nil {
		return plain(), nil
	}

	key, err := resolveKey(ctx, *c.TLS, r)

	if err != nil {
		return nil, err
	}

	return build(*c.TLS, key)
}

// Clients caches For's clients, one per connector, so each connector's
// connections are pooled across its callers. It is safe for concurrent
// use.
//
// A cached client is used while the connector's tls table and its resolved
// key stay the same; a change to either builds a new one, so an edited
// table or a `wayfindr secret set` of the key takes effect at the next For.
// Every connector without a tls table shares one client.
type Clients struct {
	resolver config.Resolver
	plain    *http.Client

	mu     sync.Mutex
	byName map[string]cached
}

// cached is one connector's client and what it was built from.
type cached struct {
	tls    config.TLS
	key    [sha256.Size]byte
	client *http.Client
}

// NewClients returns an empty cache that resolves key templates through r.
func NewClients(r config.Resolver) *Clients {
	return &Clients{resolver: r, plain: plain(), byName: map[string]cached{}}
}

// For returns connector c's client, building it as the package-level For
// does when there is none yet or when c's tls table or resolved key has
// changed since it was built. Its errors are the package-level For's.
func (cs *Clients) For(ctx context.Context, c config.Connector) (*http.Client, error) {
	if c.TLS == nil {
		cs.forget(c.Name)

		return cs.plain, nil
	}

	key, err := resolveKey(ctx, *c.TLS, cs.resolver)

	if err != nil {
		return nil, err
	}

	// A digest, so the cache holds no second copy of the key.
	sum := sha256.Sum256(key)

	cs.mu.Lock()
	defer cs.mu.Unlock()

	if e, ok := cs.byName[c.Name]; ok && config.TLSEqual(&e.tls, c.TLS) && e.key == sum {
		return e.client, nil
	}

	client, err := build(*c.TLS, key)

	if err != nil {
		return nil, err
	}

	cs.byName[c.Name] = cached{tls: *c.TLS, key: sum, client: client}

	return client, nil
}

// Retain drops the cached clients of every connector configured does not
// name, closing their idle connections. The daemon calls it on each
// reload, so a removed connector's client, and its pool, do not outlive it.
func (cs *Clients) Retain(configured config.Connectors) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for name, e := range cs.byName {
		if _, ok := configured[name]; !ok {
			e.client.CloseIdleConnections()
			delete(cs.byName, name)
		}
	}
}

func (cs *Clients) forget(name string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	delete(cs.byName, name)
}

// plain is a client on a clone of the default transport, so it keeps the
// default's proxy, timeouts, and HTTP/2 without sharing its pool.
func plain() *http.Client {
	return &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
}

// resolveKey returns the key template's value, or nil when t has none.
func resolveKey(ctx context.Context, t config.TLS, r config.Resolver) ([]byte, error) {
	if t.Key == "" {
		return nil, nil
	}

	v, err := t.Key.Resolve(ctx, r)

	if err != nil {
		return nil, fmt.Errorf("tls key: %w", err)
	}

	return []byte(v), nil
}

// build makes the client for t, with key as the client certificate's key
// when key_file is not set.
func build(t config.TLS, key []byte) (*http.Client, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if t.CAFile != "" {
		pool, err := rootsWith(t.CAFile)

		if err != nil {
			return nil, err
		}

		cfg.RootCAs = pool
	}

	if t.HasClientCert() {
		cc := &clientCert{certFile: t.CertFile, keyFile: t.KeyFile, key: key}

		// Load once now, so a bad file is For's error.
		if _, err := cc.get(nil); err != nil {
			return nil, err
		}

		cfg.GetClientCertificate = cc.get
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = cfg

	return &http.Client{Transport: tr}, nil
}

// rootsWith returns the system roots plus the certificates in caFile.
func rootsWith(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile) //nolint:gosec // G304: the path is the user's own config

	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCAFile, err)
	}

	pool, err := x509.SystemCertPool()

	if err != nil {
		pool = x509.NewCertPool()
	}

	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%w %s: no PEM certificate found", ErrCAFile, caFile)
	}

	return pool, nil
}

// clientCert serves the client certificate for each handshake, reading the
// files again when either has changed since the last read.
type clientCert struct {
	certFile string
	// keyFile is empty when the key came from a template, in key.
	keyFile string
	key     []byte

	mu   sync.Mutex
	seen [2]stamp
	cert *tls.Certificate
}

// stamp tells whether a file changed: a rewrite changes its mtime or size.
type stamp struct {
	mod  time.Time
	size int64
}

// get is the tls.Config's GetClientCertificate. An error aborts the
// handshake, and the next handshake reads the files again.
func (c *clientCert) get(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Stamps are taken before reading, so a write that races the read
	// changes them again and is read at the next handshake.
	now, err := c.stamps()

	if err != nil {
		return nil, err
	}

	if c.cert != nil && now == c.seen {
		return c.cert, nil
	}

	cert, err := c.read()

	if err != nil {
		return nil, err
	}

	c.cert, c.seen = cert, now

	return cert, nil
}

func (c *clientCert) stamps() ([2]stamp, error) {
	var s [2]stamp

	for i, f := range []string{c.certFile, c.keyFile} {
		if f == "" {
			continue
		}

		fi, err := os.Stat(f)

		if err != nil {
			return s, fmt.Errorf("%w: %w", ErrClientCert, err)
		}

		s[i] = stamp{mod: fi.ModTime(), size: fi.Size()}
	}

	return s, nil
}

func (c *clientCert) read() (*tls.Certificate, error) {
	certPEM, err := os.ReadFile(c.certFile) //nolint:gosec // G304: the path is the user's own config

	if err != nil {
		return nil, fmt.Errorf("%w: cert_file: %w", ErrClientCert, err)
	}

	keyPEM := c.key

	if c.keyFile != "" {
		//nolint:gosec // G304: the path is the user's own config
		if keyPEM, err = os.ReadFile(c.keyFile); err != nil {
			return nil, fmt.Errorf("%w: key_file: %w", ErrClientCert, err)
		}
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)

	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrClientCert, c.certFile, err)
	}

	return &cert, nil
}

// CertificateExpiry returns when connector c's client certificate stops
// being valid: the NotAfter of the first certificate in cert_file. It is
// zero, with no error, for a connector without one. The daemon's health
// check warns from it (certificate_expiring); it reads the file each time,
// so a rotated certificate's date is seen at the next check.
func CertificateExpiry(c config.Connector) (time.Time, error) {
	if c.TLS == nil || !c.TLS.HasClientCert() {
		return time.Time{}, nil
	}

	data, err := os.ReadFile(c.TLS.CertFile)

	if err != nil {
		return time.Time{}, fmt.Errorf("%w: cert_file: %w", ErrClientCert, err)
	}

	for {
		var b *pem.Block

		if b, data = pem.Decode(data); b == nil {
			return time.Time{}, fmt.Errorf("%w %s: no PEM certificate found", ErrClientCert, c.TLS.CertFile)
		}

		if b.Type != "CERTIFICATE" {
			continue
		}

		cert, err := x509.ParseCertificate(b.Bytes)

		if err != nil {
			return time.Time{}, fmt.Errorf("%w %s: %w", ErrClientCert, c.TLS.CertFile, err)
		}

		return cert.NotAfter, nil
	}
}
