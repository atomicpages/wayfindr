package fakeas

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mtlsPrefix is the path the mTLS endpoint aliases ([WithMTLSAliases]) live
// under, below the issuer's path.
const mtlsPrefix = "/mtls"

// WithTLS serves everything over TLS, with httptest's certificate for
// 127.0.0.1 ([Server.CertificatePEM] is what a client's ca_file trusts).
// With clientCAs, a client must present a certificate one of them issued
// (tls.RequireAndVerifyClientCert); with [WithMTLSAliases] too, only the
// alias endpoints require one, and the others verify one if given. The
// subject of the certificate each token and resource request presented is
// recorded (TokenRequest.ClientCert, ResourceRequest.ClientCert).
func WithTLS(clientCAs *x509.CertPool) Option {
	return func(c *config) {
		c.tls = true
		c.clientCAs = clientCAs
	}
}

// WithMTLSAliases advertises mtls_endpoint_aliases (RFC 8705 §5) for the
// token, device authorization, and registration endpoints, served under
// /mtls below the issuer. An alias answers a request without a client
// certificate with invalid_client, as a real mTLS endpoint would refuse
// the handshake. Token requests record whether they came to an alias
// (TokenRequest.MTLS).
func WithMTLSAliases() Option { return func(c *config) { c.mtlsAliases = true } }

// WithCertBoundTokens advertises tls_client_certificate_bound_access_tokens
// in the authorization server metadata and the PRM (RFC 8705 §3), and binds
// each token issued to a request that presented a client certificate to
// that certificate: the resource then answers it 401 invalid_token unless
// the request presents the same certificate.
func WithCertBoundTokens() Option { return func(c *config) { c.certBound = true } }

// listen starts the fake's server: plain HTTP, or TLS with [WithTLS].
func (s *Server) listen(t testing.TB, h http.Handler) *httptest.Server {
	t.Helper()

	if !s.cfg.tls {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)

		return srv
	}

	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12}

	if s.cfg.clientCAs != nil {
		srv.TLS.ClientCAs = s.cfg.clientCAs
		srv.TLS.ClientAuth = tls.RequireAndVerifyClientCert

		if s.cfg.mtlsAliases {
			srv.TLS.ClientAuth = tls.VerifyClientCertIfGiven
		}
	}

	// Refused handshakes are what some tests are about; keep them out of
	// the test log.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	s.CertificatePEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	return srv
}

// serveMTLSAliases adds the alias endpoints to mux under the issuer path.
func (s *Server) serveMTLSAliases(mux *http.ServeMux) {
	base := s.cfg.issuerPath + mtlsPrefix
	s.MTLSTokenEndpoint = s.Issuer + mtlsPrefix + "/token"
	s.MTLSDeviceAuthorizationEndpoint = s.Issuer + mtlsPrefix + "/device_authorization"
	mux.HandleFunc("POST "+base+"/token", s.requireCert(s.serveToken))
	mux.HandleFunc("POST "+base+"/device_authorization", s.requireCert(s.serveDeviceAuthorization))

	if !s.cfg.noDCR {
		s.MTLSRegistrationEndpoint = s.Issuer + mtlsPrefix + "/register"
		mux.HandleFunc("POST "+base+"/register", s.requireCert(s.serveRegister))
	}
}

// mtlsAliases is the metadata's mtls_endpoint_aliases, or nil without
// [WithMTLSAliases].
func (s *Server) mtlsAliases() map[string]string {
	if !s.cfg.mtlsAliases {
		return nil
	}

	m := map[string]string{
		"token_endpoint":                s.MTLSTokenEndpoint,
		"device_authorization_endpoint": s.MTLSDeviceAuthorizationEndpoint,
	}

	if s.MTLSRegistrationEndpoint != "" {
		m["registration_endpoint"] = s.MTLSRegistrationEndpoint
	}

	return m
}

// requireCert refuses a request to an alias that presents no client
// certificate.
func (s *Server) requireCert(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if peerCN(r) == "" {
			writeJSON(w, http.StatusUnauthorized, oauthError{"invalid_client", "the mTLS endpoint needs a client certificate"})

			return
		}

		h(w, r)
	}
}

// viaAlias reports whether r came to an mTLS endpoint alias.
func viaAlias(r *http.Request) bool { return strings.Contains(r.URL.Path, mtlsPrefix+"/") }

// peerCN is the common name of the client certificate r presented, or "".
func peerCN(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}

	return r.TLS.PeerCertificates[0].Subject.CommonName
}

// certThumbprint is RFC 8705 §3.1's x5t#S256 of the client certificate r
// presented, or "" when it presented none.
func certThumbprint(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}

	sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// binding is the certificate a token issued to r is bound to: its
// thumbprint with [WithCertBoundTokens], else "".
func (s *Server) binding(r *http.Request) string {
	if !s.cfg.certBound {
		return ""
	}

	return certThumbprint(r)
}
