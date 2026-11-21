package auth_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/httpclient"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/testutil/testcert"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// The tests in this file cover a connector's [tls] table (design §4, §7.3,
// RFC 8705) at the broker's seams: one HTTP client per connector, from
// httpclient.Clients, carries its auth flow, its refreshes, and, through
// upstream, its MCP traffic, against a fake authorization server served
// over TLS.

// tlsHarness is a brokerHarness whose fake serves over TLS, whose
// connector has a tls table, and whose broker gets each connector's HTTP
// client from clients, as the daemon wires it.
type tlsHarness struct {
	*brokerHarness
	ca       *testcert.CA
	dir      string
	clients  *httpclient.Clients
	certFile string
	keyFile  string
}

// newTLSHarness starts the fake with opts (WithTLS among them) and gives
// the connector a tls table trusting the fake's certificate, with a client
// certificate issued by ca for "client-1" when clientCert is set.
func newTLSHarness(t *testing.T, ca *testcert.CA, clientCert bool, opts ...fakeas.Option) *tlsHarness {
	t.Helper()
	h := &tlsHarness{brokerHarness: newBrokerHarness(t, opts...), ca: ca, dir: t.TempDir()}
	h.conn.TLS = &config.TLS{CAFile: testcert.Write(t, h.dir, "ca.pem", h.as.CertificatePEM)}

	if clientCert {
		h.issue(t, "client-1")
		h.conn.TLS.CertFile, h.conn.TLS.KeyFile = h.certFile, h.keyFile
	}

	h.clients = httpclient.NewClients(h.opts.Resolver)
	h.opts.Clients = h.clients
	h.broker = auth.NewBroker(h.opts)

	return h
}

// issue writes a client certificate for cn, and its key, over the files the
// tls table names, so the next handshake presents it.
func (h *tlsHarness) issue(t *testing.T, cn string) {
	t.Helper()
	certPEM, keyPEM := h.ca.Issue(t, cn, false)
	h.certFile = testcert.Write(t, h.dir, "client.pem", certPEM)
	h.keyFile = testcert.Write(t, h.dir, "client.key", keyPEM)
	// A coarse filesystem clock could leave the mtime unchanged.
	later := time.Now().Add(time.Duration(len(cn)) * time.Hour)

	for _, f := range []string{h.certFile, h.keyFile} {
		if err := os.Chtimes(f, later, later); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
}

// http is the connector's HTTP client, the one the broker uses.
func (h *tlsHarness) http(t *testing.T) *http.Client {
	t.Helper()
	hc, err := h.clients.For(t.Context(), h.conn)

	if err != nil {
		t.Fatalf("connector client: %v", err)
	}

	return hc
}

// authorize runs an auth flow, playing the browser through the
// connector's own client (it trusts the fake, and has the certificate the
// fake may ask for), and returns how it ended.
func (h *tlsHarness) authorize(t *testing.T) auth.FlowResult {
	t.Helper()
	f := h.start(t)
	browser := &http.Client{
		Transport:     h.http(t).Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := send(t, browser, http.MethodGet, f.AuthorizeURL, "")

	if err != nil {
		t.Fatalf("authorize: %v", err)
	}

	_ = resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))

	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize = %d %q, want a redirect", resp.StatusCode, resp.Header.Get("Location"))
	}

	if code, page := h.callback(t, loc); code != http.StatusOK {
		t.Fatalf("callback = %d: %s", code, page)
	}

	return h.result(t, f.ID)
}

// get GETs the resource through the broker's Transport over the
// connector's client, as upstream sends MCP traffic.
func (h *tlsHarness) get(t *testing.T) (int, error) {
	t.Helper()
	client := &http.Client{Transport: h.broker.Transport(h.conn, h.http(t).Transport)}
	resp, err := send(t, client, http.MethodGet, h.as.ResourceURL, "")

	if err != nil {
		return 0, err
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	return resp.StatusCode, nil
}

// manager is an upstream.Manager wired to the broker and the clients as the
// daemon wires them.
func (h *tlsHarness) manager(t *testing.T) *upstream.Manager {
	t.Helper()
	m := upstream.New(upstream.Options{
		Logger:        zerolog.New(zerolog.NewTestWriter(t)),
		AuthTransport: h.broker.Transport,
		Clients:       h.clients,
	})
	t.Cleanup(func() { _ = m.Close(context.Background()) })

	return m
}

// mcpBehind is a resource handler that proxies to a fake MCP server, so a
// connector behind the fake authorization server connects.
func mcpBehind(t *testing.T) fakeas.Option {
	t.Helper()
	target, err := url.Parse(fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL)

	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1

	return fakeas.WithResourceHandler(proxy)
}

// tokenCerts are the client certificates the fake's token requests of
// grant presented, and whether each came to the mTLS alias.
func tokenCerts(as *fakeas.Server, grant string) []string {
	var out []string

	for _, tr := range as.TokenRequests() {
		if tr.GrantType == grant {
			alias := ""

			if tr.MTLS {
				alias = " (mtls alias)"
			}

			out = append(out, tr.AuthMethod+" "+tr.ClientCert+alias)
		}
	}

	return out
}

// TestTLSClientAuthUsesTheMTLSAliasAndTheCertificate: with
// tls_client_auth, the token request goes to the mTLS alias of the token
// endpoint with the client certificate as the client's credential; the
// token is bound to that certificate, and the MCP connection presents the
// same one, so upstream connects.
func TestTLSClientAuthUsesTheMTLSAliasAndTheCertificate(t *testing.T) {
	ca := testcert.NewCA(t)
	h := newTLSHarness(t, ca, true,
		fakeas.WithTLS(ca.Pool()), fakeas.WithMTLSAliases(), fakeas.WithCertBoundTokens(), mcpBehind(t))
	h.conn.Auth.TokenEndpointAuthMethod = config.AuthMethodTLSClientAuth

	if res := h.authorize(t); res.State != auth.FlowDone {
		t.Fatalf("flow = %s (%v), want done", res.State, res.Err)
	}

	if diff := cmp.Diff([]string{"tls_client_auth client-1 (mtls alias)"}, tokenCerts(h.as, "authorization_code")); diff != "" {
		t.Errorf("code exchanges (-want +got):\n%s", diff)
	}

	m := h.manager(t)
	apply(t, m, config.Connectors{"linear": h.conn})

	if got := m.Status(); len(got) != 1 || got[0].State != upstream.StateReady {
		t.Fatalf("Status = %+v, want linear ready", got)
	}

	for _, r := range h.as.ResourceRequests() {
		if r.Bearer && r.ClientCert != "client-1" {
			t.Errorf("a resource request with the token presented client certificate %q, want client-1", r.ClientCert)
		}
	}
}

// TestPrivateCAFromCAFileWorksEndToEnd: a server on a private CA is
// discovered, authorized, and connected to with ca_file alone.
func TestPrivateCAFromCAFileWorksEndToEnd(t *testing.T) {
	h := newTLSHarness(t, testcert.NewCA(t), false, fakeas.WithTLS(nil), mcpBehind(t))

	if res := h.authorize(t); res.State != auth.FlowDone {
		t.Fatalf("flow = %s (%v), want done", res.State, res.Err)
	}

	m := h.manager(t)
	apply(t, m, config.Connectors{"linear": h.conn})

	if got := m.Status(); len(got) != 1 || got[0].State != upstream.StateReady {
		t.Fatalf("Status = %+v, want linear ready", got)
	}
}

// TestTLSFailuresAtTheAuthorizationServerAreConfigErrors: an authorization
// server whose certificate is not trusted is tls_untrusted, and one that
// refuses the handshake for want of a client certificate is tls_handshake;
// both are config errors, which upstream reads as auth_config.
func TestTLSFailuresAtTheAuthorizationServerAreConfigErrors(t *testing.T) {
	ca := testcert.NewCA(t)
	tests := []struct {
		name  string
		setup func(t *testing.T) *tlsHarness
		want  auth.Case
		up    upstream.Case
	}{
		{"untrusted", func(t *testing.T) *tlsHarness {
			h := newTLSHarness(t, ca, false, fakeas.WithTLS(nil))
			h.conn.TLS = nil

			return h
		}, auth.CaseTLSUntrusted, upstream.CaseTLSUntrusted},
		{"no client certificate", func(t *testing.T) *tlsHarness {
			return newTLSHarness(t, ca, false, fakeas.WithTLS(ca.Pool()))
		}, auth.CaseTLSHandshake, upstream.CaseTLSHandshake},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.setup(t)
			_, err := h.flows().Start(t.Context(), "linear", auth.StartOptions{})

			if e, ok := auth.AsError(err); !ok || e.Case != tt.want || e.Kind != auth.KindConfig {
				t.Fatalf("Start error = %v, want config error %s", err, tt.want)
			}

			if a := auth.Advise(&auth.Error{Case: tt.want}); !strings.Contains(a.Action, "[tls]") {
				t.Errorf("advice %+v, want it to point at the [tls] table", a)
			}

			// A refresh meets the same failure on the MCP connection's
			// behalf: upstream reads it as auth_config.
			if err := auth.SeedToken(t.Context(), h.broker, "linear", h.as.ResourceURL, h.as.TokenEndpoint, "at", "rt",
				h.clock.Now().Add(-time.Minute), ""); err != nil {
				t.Fatalf("seed token: %v", err)
			}

			m := h.manager(t)
			apply(t, m, config.Connectors{"linear": h.conn})
			_, err = m.Call(t.Context(), "linear", "echo", nil)

			if e, ok := upstream.AsError(err); !ok || e.Kind != upstream.KindAuthConfig || e.Case != tt.up {
				t.Errorf("Call error = %v, want auth_config %s", err, tt.up)
			}
		})
	}
}

// TestRotatedClientCertificateIsUsedForTheNextRefresh covers scenario 3: a
// client certificate rotated on disk mid-session is presented at the next
// handshake, to the token endpoint and the resource alike, without a new
// client or a restart.
func TestRotatedClientCertificateIsUsedForTheNextRefresh(t *testing.T) {
	ca := testcert.NewCA(t)
	h := newTLSHarness(t, ca, true, fakeas.WithTLS(ca.Pool()))

	if res := h.authorize(t); res.State != auth.FlowDone {
		t.Fatalf("flow = %s (%v), want done", res.State, res.Err)
	}

	hc := h.http(t)
	h.issue(t, "client-2")
	// Pooled connections finished their handshake with the old
	// certificate; a new connection makes a new one.
	hc.CloseIdleConnections()
	h.as.ExpireAccessTokens()

	if code, err := h.get(t); err != nil || code != http.StatusOK {
		t.Fatalf("get = %d, %v; want 200 after a refresh", code, err)
	}

	if h.http(t) != hc {
		t.Error("the rotation built a new client, want the cached one")
	}

	want := []string{"tls_client_auth client-1", "tls_client_auth client-2"}
	got := append(tokenCerts(h.as, "authorization_code"), tokenCerts(h.as, "refresh_token")...)

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("token requests (-want +got):\n%s", diff)
	}

	if rs := h.as.ResourceRequests(); len(rs) == 0 || rs[len(rs)-1].ClientCert != "client-2" {
		t.Errorf("resource requests = %+v, want the last to present client-2", rs)
	}
}

// TestTLSEditCancelsAPendingFlow covers scenario 3: a change to the
// connector's [tls] table ends its pending flow, whose token would be bound
// to the old certificate, as a url or [auth] change does.
func TestTLSEditCancelsAPendingFlow(t *testing.T) {
	h := newBrokerHarness(t)
	dir := t.TempDir()
	ca := testcert.NewCA(t)
	h.conn.TLS = &config.TLS{CAFile: testcert.Write(t, dir, "a.pem", ca.PEM)}
	f := h.start(t)

	h.conn.TLS = &config.TLS{CAFile: testcert.Write(t, dir, "b.pem", ca.PEM)}
	h.flows().ConfigChanged()

	res := h.result(t, f.ID)
	e, ok := auth.AsError(res.Err)

	if res.State != auth.FlowFailed || !ok || e.Case != auth.CaseFlowCancelled || e.Detail != "the connector's [tls] table changed" {
		t.Errorf("flow = %s (%v), want failed: flow_cancelled because the [tls] table changed", res.State, res.Err)
	}
}

// TestMissingTLSKeySecretIsMissingValue: a tls key that names a secret
// that isn't stored reads as missing_value naming it, as any other
// reference does, not as an unreachable upstream.
func TestMissingTLSKeySecretIsMissingValue(t *testing.T) {
	ca := testcert.NewCA(t)
	h := newTLSHarness(t, ca, true, fakeas.WithTLS(ca.Pool()))
	h.conn.TLS.KeyFile, h.conn.TLS.Key = "", "${secret:linear_tls_key}"
	m := h.manager(t)
	apply(t, m, config.Connectors{"linear": h.conn})

	s := m.Status()[0]
	want := []any{upstream.StateAuthError, upstream.KindMissingValue, upstream.CaseSecret, "secret:linear_tls_key"}

	if diff := cmp.Diff(want, []any{s.State, s.Kind, s.Case, s.Ref}); diff != "" {
		t.Errorf("State, Kind, Case, Ref (-want +got):\n%s", diff)
	}
}

// TestTokenRejectedPointsAtCertificateBoundTokens: when the upstream
// rejects a fresh token, its resource metadata advertises
// certificate-bound tokens, and the connector has no client certificate,
// the token_rejected detail says to add one in [tls]. The flag means the
// resource supports such tokens, so nothing is refused before a call.
func TestTokenRejectedPointsAtCertificateBoundTokens(t *testing.T) {
	reject := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		w.WriteHeader(http.StatusUnauthorized)
	})

	for _, bound := range []bool{true, false} {
		opts := []fakeas.Option{fakeas.WithResourceHandler(reject)}

		if bound {
			opts = append(opts, fakeas.WithCertBoundTokens())
		}

		h := newBrokerHarness(t, opts...)
		h.seed(t, "", time.Hour)
		_, err := h.get(t)
		e, ok := auth.AsError(err)

		if !ok || e.Case != auth.CaseTokenRejected {
			t.Fatalf("bound %t: get error = %v, want token_rejected", bound, err)
		}

		if got := strings.Contains(e.Detail, "[connectors.linear.tls]"); got != bound {
			t.Errorf("bound %t: detail %q mentions [connectors.linear.tls]: %t, want %t", bound, e.Detail, got, bound)
		}
	}
}

// TestBrokerWithoutClientsUsesItsClient: without Clients, a connector's tls
// table plays no part in the broker's requests (BrokerOptions.Client), as
// before the table existed.
func TestBrokerWithoutClientsUsesItsClient(t *testing.T) {
	h := newBrokerHarness(t)
	h.conn.TLS = &config.TLS{CAFile: "/nonexistent/ca.pem"}

	if res := h.start(t); res.AuthorizeURL == "" {
		t.Fatal("Start gave no authorize URL")
	}
}

// TestClientCredentialsWithTLSClientAuth: an unattended grant gets its
// token over the connector's own HTTP client too: discovery trusts its
// ca_file, and the client_credentials grant goes to the mTLS alias with the
// client certificate as the client's credential, the same certificate the
// resource request presents.
func TestClientCredentialsWithTLSClientAuth(t *testing.T) {
	ca := testcert.NewCA(t)
	h := newTLSHarness(t, ca, true, fakeas.WithTLS(ca.Pool()), fakeas.WithMTLSAliases(), fakeas.WithCertBoundTokens())
	h.conn.Auth.Grant = config.GrantClientCredentials
	h.conn.Auth.TokenEndpointAuthMethod = config.AuthMethodTLSClientAuth

	if code, err := h.get(t); err != nil || code != http.StatusOK {
		t.Fatalf("get = %d, %v; want 200", code, err)
	}

	if diff := cmp.Diff([]string{"tls_client_auth client-1 (mtls alias)"}, tokenCerts(h.as, fakeas.GrantClientCredentials)); diff != "" {
		t.Errorf("client_credentials grants (-want +got):\n%s", diff)
	}

	if rs := h.as.ResourceRequests(); len(rs) == 0 || rs[len(rs)-1].ClientCert != "client-1" {
		t.Errorf("resource requests = %+v, want the last to present client-1", rs)
	}
}
