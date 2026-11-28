package auth_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// The tests in this file cover DPoP (RFC 9449, design §7.3): the grant's
// key, the proofs on token and upstream requests, and the nonces, against
// fakeas, which verifies every proof with [fakeas.WithDPoP].

// newDPoPHarness is a broker harness whose connector's dpop is mode. Its
// clock is the real one: the fake checks a proof's iat against its own.
func newDPoPHarness(t *testing.T, mode config.DPoPMode, opts ...fakeas.Option) *brokerHarness {
	t.Helper()
	h := newBrokerHarness(t, opts...)
	h.clock.t = time.Now()
	h.conn.Auth.DPoP = mode

	return h
}

// authorizeFlow runs an authorization code flow to its end and wants it
// done.
func (h *brokerHarness) authorizeFlow(t *testing.T) {
	t.Helper()
	f := h.start(t)

	if status, page := h.callback(t, consent(t, f.AuthorizeURL)); status != http.StatusOK {
		t.Fatalf("callback = %d %q, want 200", status, page)
	}
}

// grantProofs are the DPoP proofs of the token requests of grant type
// grant the fake saw, nil for one without a proof, with each request's
// error.
func grantProofs(as *fakeas.Server, grant string) ([]*fakeas.DPoPProof, []string) {
	var (
		proofs []*fakeas.DPoPProof
		errs   []string
	)

	for _, tr := range as.TokenRequests() {
		if tr.GrantType == grant {
			proofs, errs = append(proofs, tr.DPoP), append(errs, tr.Error)
		}
	}

	return proofs, errs
}

// resourceProofs are the DPoP proofs of the requests that reached the
// resource with a token; probes, which carry none, are left out.
func resourceProofs(as *fakeas.Server) []*fakeas.DPoPProof {
	var out []*fakeas.DPoPProof

	for _, r := range as.ResourceRequests() {
		if r.DPoP != nil || r.Bearer {
			out = append(out, r.DPoP)
		}
	}

	return out
}

// wantGoodProof wants p to be a proof the fake accepted, for a method
// request to target, with nonce, and with an ath exactly when resource.
func wantGoodProof(t *testing.T, p *fakeas.DPoPProof, method, target, nonce string, resource bool) {
	t.Helper()

	switch {
	case p == nil:
		t.Fatalf("no DPoP proof; want one for %s %s", method, target)
	case p.Error != "":
		t.Errorf("proof refused: %s", p.Error)
	case p.Typ != "dpop+jwt" || p.Alg != "ES256" || p.JKT == "" || p.PrivateKeyInJWK:
		t.Errorf("proof header = %+v, want typ dpop+jwt, ES256, and a public P-256 jwk", p)
	case p.HTM != method || p.HTU != target || p.Nonce != nonce:
		t.Errorf("proof htm, htu, nonce = %q %q %q; want %q %q %q", p.HTM, p.HTU, p.Nonce, method, target, nonce)
	case (p.ATH != "") != resource:
		t.Errorf("proof ath = %q; want one only on a resource request", p.ATH)
	}
}

// An auth flow with DPoP: the authorize request names the key, the code
// exchange proves it, the token is bound to it, and upstream requests
// carry it with the DPoP scheme. The key survives storage: a new broker on
// the same store sends the stored token with it.
func TestDPoPFlowBindsTheTokenToTheGrantsKey(t *testing.T) {
	h := newDPoPHarness(t, config.DPoPAuto, fakeas.WithDPoP(false))
	h.authorizeFlow(t)

	proofs, errs := grantProofs(h.as, "authorization_code")

	if diff := cmp.Diff([]string{""}, errs); diff != "" {
		t.Fatalf("code exchanges (-want +got):\n%s", diff)
	}

	wantGoodProof(t, proofs[0], http.MethodPost, h.as.TokenEndpoint, "", false)
	jkt := proofs[0].JKT

	if got := h.lastAuthorization(t).DPoPJKT; got != jkt {
		t.Errorf("authorize dpop_jkt = %q, want the code exchange's key %q", got, jkt)
	}

	h.mustGet(t)

	// The daemon restarts: the key comes back from the sealed token.
	h.broker = auth.NewBroker(h.opts)
	h.mustGet(t)

	rs := resourceProofs(h.as)

	if len(rs) != 2 {
		t.Fatalf("resource requests with a token = %d, want 2", len(rs))
	}

	for _, p := range rs {
		wantGoodProof(t, p, http.MethodGet, h.as.ResourceURL, "", true)

		if p.JKT != jkt {
			t.Errorf("resource proof key = %q, want the grant's %q", p.JKT, jkt)
		}
	}

	for _, r := range h.as.ResourceRequests() {
		if r.Bearer {
			t.Errorf("a resource request carried a Bearer token; want the DPoP scheme only")
		}
	}
}

// A refresh proves with the grant's key, read from the stored token, and
// the new token is bound to it too; the replay after the 401 carries a
// fresh proof.
func TestDPoPRefreshKeepsTheGrantsKey(t *testing.T) {
	h := newDPoPHarness(t, config.DPoPAuto, fakeas.WithDPoP(false), fakeas.WithRotatingRefreshTokens())
	h.authorizeFlow(t)
	h.mustGet(t)
	h.as.ExpireAccessTokens()
	h.broker = auth.NewBroker(h.opts) // the key is read from the store
	h.mustGet(t)

	code, _ := grantProofs(h.as, "authorization_code")
	refresh, errs := grantProofs(h.as, "refresh_token")

	if diff := cmp.Diff([]string{""}, errs); diff != "" {
		t.Fatalf("refresh grants (-want +got):\n%s", diff)
	}

	wantGoodProof(t, refresh[0], http.MethodPost, h.as.TokenEndpoint, "", false)

	if refresh[0].JKT != code[0].JKT {
		t.Errorf("refresh proof key = %q, want the grant's %q", refresh[0].JKT, code[0].JKT)
	}

	// The token before the refresh, the one refused, and the replay.
	rs := resourceProofs(h.as)
	jtis := map[string]bool{}

	for _, p := range rs {
		if p == nil || p.JKT != code[0].JKT {
			t.Fatalf("resource proofs = %+v, want each by the grant's key", rs)
		}

		jtis[p.JTI] = true
	}

	if len(rs) != 3 || len(jtis) != 3 {
		t.Errorf("resource proofs = %d with %d distinct jtis, want 3 and 3: a replay carries a fresh proof", len(rs), len(jtis))
	}

	if p := rs[2]; p.Error != "" {
		t.Errorf("replay proof refused: %s", p.Error)
	}
}

// Scenario 4: an MCP server that is its own authorization server, on one
// origin, demands a nonce of each role. Each demand is answered by one
// retry with that role's nonce, which is then sent from the start, so
// neither clobbers the other.
func TestDPoPNoncesOfEachRoleOnOneOrigin(t *testing.T) {
	h := newDPoPHarness(t, config.DPoPAuto, fakeas.WithDPoP(true))

	if !strings.HasPrefix(h.as.TokenEndpoint, h.as.URL) || !strings.HasPrefix(h.as.ResourceURL, h.as.URL) {
		t.Fatalf("token endpoint %s and resource %s are not on one origin", h.as.TokenEndpoint, h.as.ResourceURL)
	}

	h.authorizeFlow(t)
	h.mustGet(t)
	h.as.ExpireAccessTokens()
	h.mustGet(t)

	code, codeErrs := grantProofs(h.as, "authorization_code")

	if diff := cmp.Diff([]string{"use_dpop_nonce", ""}, codeErrs); diff != "" {
		t.Fatalf("code exchanges (-want +got):\n%s", diff)
	}

	if code[0].Nonce != "" || code[1].Nonce != fakeas.ASNonce || code[0].JTI == code[1].JTI {
		t.Errorf("code exchange nonces = %q, %q (jtis %q, %q); want none, then %q with a new jti",
			code[0].Nonce, code[1].Nonce, code[0].JTI, code[1].JTI, fakeas.ASNonce)
	}

	refresh, refreshErrs := grantProofs(h.as, "refresh_token")

	if diff := cmp.Diff([]string{""}, refreshErrs); diff != "" {
		t.Errorf("refresh grants (-want +got): the known nonce needs no retry\n%s", diff)
	}

	wantGoodProof(t, refresh[0], http.MethodPost, h.as.TokenEndpoint, fakeas.ASNonce, false)

	var nonces []string

	for _, p := range resourceProofs(h.as) {
		nonces = append(nonces, p.Nonce)
	}

	// The first request has no nonce and is told the resource's; the
	// expired token is refused with the nonce, and its replay after the
	// refresh carries it too.
	want := []string{"", fakeas.RSNonce, fakeas.RSNonce, fakeas.RSNonce}

	if diff := cmp.Diff(want, nonces); diff != "" {
		t.Errorf("resource proof nonces (-want +got):\n%s", diff)
	}
}

// A token request answered with use_dpop_nonce is not sent again once its
// ctx has ended: the resend could be answered with a token no one reads.
// The callback's request ends as the first answer arrives. Token requests
// are counted where they leave Wayfindr, before net/http, which does not
// promise to refuse a request whose context has ended (wayfindr-1j97).
func TestDPoPNonceResendIsNotSentOnceTheRequestHasEnded(t *testing.T) {
	h := newDPoPHarness(t, config.DPoPAuto, fakeas.WithDPoP(true))
	ctx, cancel := context.WithCancel(t.Context())
	var sent atomic.Int64

	hookTokenEndpoint(h, func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		resp, err := http.DefaultTransport.RoundTrip(r)
		cancel()

		return resp, err
	})
	f := h.start(t)

	if status, page := h.callbackCtx(ctx, consent(t, f.AuthorizeURL)); status == http.StatusOK {
		t.Fatalf("callback = %d %q, want a failure", status, page)
	}

	if diff := cmp.Diff([]string{"use_dpop_nonce"}, h.codeExchanges()); diff != "" {
		t.Errorf("code exchanges (-want +got):\n%s", diff)
	}

	if n := sent.Load(); n != 1 {
		t.Errorf("token requests sent = %d, want 1: the resend's ctx had ended", n)
	}
}

// An authorization server that does not bind tokens (token_type Bearer)
// gets proofs it ignores, and the token goes upstream as a bearer token.
func TestDPoPTokenTheServerDidNotBindIsSentAsBearer(t *testing.T) {
	h := newDPoPHarness(t, config.DPoPAlways)
	h.authorizeFlow(t)
	h.mustGet(t)

	if proofs, _ := grantProofs(h.as, "authorization_code"); len(proofs) != 1 || proofs[0] == nil {
		t.Errorf("code exchange proofs = %v, want one: dpop is always", proofs)
	}

	reqs := h.as.ResourceRequests()

	if last := reqs[len(reqs)-1]; !last.Bearer || last.DPoP != nil {
		t.Errorf("resource request = %+v, want a Bearer token without a proof", last)
	}
}

// dpop chooses whether a grant uses DPoP: never, always (unless the server
// cannot verify ES256), and auto when the resource asks for it and the
// server can.
func TestDPoPModeChoosesTheSender(t *testing.T) {
	tests := []struct {
		name  string
		mode  config.DPoPMode
		opts  []fakeas.Option
		proof bool
	}{
		{"auto, asked", config.DPoPAuto, []fakeas.Option{fakeas.WithDPoP(false)}, true},
		{"unset is auto", "", []fakeas.Option{fakeas.WithDPoP(false)}, true},
		{"auto, not asked", config.DPoPAuto, []fakeas.Option{fakeas.WithDPoPAlgs("ES256")}, false},
		{"auto, asked without ES256", config.DPoPAuto, []fakeas.Option{fakeas.WithDPoP(false), fakeas.WithDPoPAlgs("RS256")}, false},
		{"never, asked", config.DPoPNever, []fakeas.Option{fakeas.WithDPoP(false)}, false},
		{"always, no algs listed", config.DPoPAlways, nil, true},
		{"always, ES256 listed", config.DPoPAlways, []fakeas.Option{fakeas.WithDPoPAlgs("RS256", "ES256")}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newDPoPHarness(t, tt.mode, tt.opts...)
			f := h.start(t)
			consent(t, f.AuthorizeURL)

			if got := h.lastAuthorization(t).DPoPJKT != ""; got != tt.proof {
				t.Errorf("authorize request has dpop_jkt = %v, want %v", got, tt.proof)
			}
		})
	}
}

// dpop = "always" with a server that lists signing algorithms without
// ES256 cannot work: the flow fails as it starts, before the user consents.
func TestDPoPAlwaysWithoutES256IsAConfigError(t *testing.T) {
	h := newDPoPHarness(t, config.DPoPAlways, fakeas.WithDPoPAlgs("RS256"))
	_, err := h.flows().Start(t.Context(), h.conn.Name, auth.StartOptions{})

	wantConfigError(t, err, auth.CaseDPoPAlgUnsupported)

	if n := len(h.as.Authorizations()) + len(h.as.TokenRequests()); n != 0 {
		t.Errorf("the authorization server saw %d requests, want none", n)
	}

	a := auth.Advise(mustError(t, err))

	if !strings.Contains(a.Action, `dpop = "auto"`) {
		t.Errorf("advice = %+v, want it to name dpop", a)
	}
}

// mustError narrows err to an auth error.
func mustError(t *testing.T, err error) *auth.Error {
	t.Helper()
	e, ok := auth.AsError(err)

	if !ok {
		t.Fatalf("err = %v, want an auth error", err)
	}

	return e
}

// An unattended grant uses DPoP too: its key is made with the in-memory
// entry and kept for the next grant.
func TestDPoPClientCredentials(t *testing.T) {
	h := newUnattendedHarness(t, fakeas.WithDPoP(true))
	h.clock.t = time.Now()
	h.mustGet(t)
	h.as.ExpireAccessTokens()
	h.mustGet(t)

	proofs, errs := grantProofs(h.as, fakeas.GrantClientCredentials)

	// The first grant learns the nonce; the second sends it.
	if diff := cmp.Diff([]string{"use_dpop_nonce", "", ""}, errs); diff != "" {
		t.Fatalf("client_credentials grants (-want +got):\n%s", diff)
	}

	wantGoodProof(t, proofs[2], http.MethodPost, h.as.TokenEndpoint, fakeas.ASNonce, false)

	if proofs[0].JKT != proofs[2].JKT {
		t.Errorf("grant keys = %q, %q; want one key for the grant", proofs[0].JKT, proofs[2].JKT)
	}

	rs := resourceProofs(h.as)

	if last := rs[len(rs)-1]; last == nil || last.Error != "" || last.JKT != proofs[0].JKT {
		t.Errorf("last resource proof = %+v, want a good one by the grant's key", last)
	}

	names, err := h.store.ListCredentialConnectors(t.Context(), h.user)

	if err != nil || len(names) != 0 {
		t.Errorf("credentials = %q, %v; want none stored", names, err)
	}
}

// A device code flow proves every poll with the grant's key, a fresh proof
// each; the device authorization request carries none.
func TestDPoPDevicePollsCarryProofs(t *testing.T) {
	h := newDPoPHarness(t, config.DPoPAuto, fakeas.WithDeviceCode(), fakeas.WithDPoP(false))
	g := gatePolls(h)
	t.Cleanup(h.broker.Close)

	f := h.startDevice(t)

	for range 2 {
		g.next(t)
		g.poll()
	}

	g.next(t)
	h.as.ApproveDevice(f.UserCode)
	g.poll()

	if res := h.waitDone(t, f.ID); res.State != auth.FlowDone || res.Err != nil {
		t.Fatalf("flow = %+v, want done", res)
	}

	if reqs := h.as.DeviceAuthorizations(); len(reqs) != 1 || reqs[0].DPoP {
		t.Errorf("device authorization requests = %+v, want one without a proof", reqs)
	}

	proofs, errs := grantProofs(h.as, fakeas.GrantDeviceCode)

	if diff := cmp.Diff([]string{"authorization_pending", "authorization_pending", ""}, errs); diff != "" {
		t.Fatalf("device code grants (-want +got):\n%s", diff)
	}

	jtis := map[string]bool{}

	for _, p := range proofs {
		wantGoodProof(t, p, http.MethodPost, h.as.TokenEndpoint, "", false)
		jtis[p.JTI] = true

		if p.JKT != proofs[0].JKT {
			t.Errorf("poll keys differ: %q, %q", p.JKT, proofs[0].JKT)
		}
	}

	if len(jtis) != len(proofs) {
		t.Errorf("poll jtis = %d distinct of %d, want a fresh proof for each poll", len(jtis), len(proofs))
	}

	h.mustGet(t)
}

// A token stored before DPoP has no key in its sealed blob: it still loads,
// goes upstream as a bearer token, and refreshes without a proof.
func TestDPoPTokenStoredWithoutAKeyIsBearer(t *testing.T) {
	h := newDPoPHarness(t, config.DPoPAuto, fakeas.WithDPoPAlgs("ES256"))
	h.seed(t, "", time.Hour)
	h.mustGet(t)
	h.as.ExpireAccessTokens()
	h.mustGet(t)

	if proofs, errs := grantProofs(h.as, "refresh_token"); len(proofs) != 1 || proofs[0] != nil || errs[0] != "" {
		t.Errorf("refresh grants = %v %v, want one without a proof", proofs, errs)
	}

	for _, r := range h.as.ResourceRequests() {
		if r.DPoP != nil {
			t.Errorf("resource request %+v carried a proof; want bearer only", r)
		}
	}
}

// An authorization server that rejects the proof (invalid_dpop_proof)
// puts the connector in auth_error: neither a retry nor a new auth flow
// would carry a better one.
func TestDPoPRejectedProofIsAConfigError(t *testing.T) {
	h := newUnattendedHarness(t, fakeas.WithDPoP(false))
	h.clock.t = time.Now()
	h.as.FailTokenRequests("invalid_dpop_proof")

	_, err := h.get(t)

	if code := wantConfigError(t, err, auth.CaseDPoPRejected); code != "invalid_dpop_proof" {
		t.Errorf("provider code = %q, want invalid_dpop_proof", code)
	}
}
