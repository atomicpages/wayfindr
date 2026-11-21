package auth_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// The tests in this file run the device code flow (RFC 8628, design §7.3)
// at the flow entry's seam (auth.Flows): Start, the user's approval at the
// fake authorization server, and the poller, which a pollGate steps by
// hand in place of its timer.

// pollGate stands in for the device code poller's timer: each wait hands
// the test its duration and blocks until the test lets one poll through.
type pollGate struct {
	waits   chan time.Duration
	release chan struct{}
}

// gatePolls makes h's broker wait on a new gate between polls.
func gatePolls(h *brokerHarness) *pollGate {
	g := &pollGate{waits: make(chan time.Duration), release: make(chan struct{})}
	auth.SetPollSleep(h.broker, g.sleep)

	return g
}

func (g *pollGate) sleep(ctx context.Context, d time.Duration) error {
	select {
	case g.waits <- d:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// next waits until the poller waits for its next poll, so its last poll
// is done, and returns how long it would wait.
func (g *pollGate) next(t *testing.T) time.Duration {
	t.Helper()

	select {
	case d := <-g.waits:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("the poller did not wait for its next poll")

		return 0
	}
}

// poll lets the waiting poller poll once.
func (g *pollGate) poll() { g.release <- struct{}{} }

// startDevice starts a device code flow for the connector's current config.
func (h *brokerHarness) startDevice(t *testing.T) auth.Flow {
	t.Helper()
	f, err := h.flows().Start(t.Context(), h.conn.Name, auth.StartOptions{Device: true})

	if err != nil {
		t.Fatalf("Start(device): %v", err)
	}

	return f
}

// waitDone waits for the flow to end and returns its result.
func (h *brokerHarness) waitDone(t *testing.T, id string) auth.FlowResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	res, ok := h.flows().Wait(ctx, id)

	if !ok || res.State == auth.FlowPending {
		t.Fatalf("Wait(%s) = %+v, %v; want the flow ended", id, res, ok)
	}

	return res
}

// deviceGrants are the device code grants the fake saw, by error.
func (h *brokerHarness) deviceGrants() []string {
	var out []string

	for _, tr := range h.as.TokenRequests() {
		if tr.GrantType == fakeas.GrantDeviceCode {
			out = append(out, tr.Error)
		}
	}

	return out
}

func TestDeviceFlowStoresTheTokenOnceTheUserApproves(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithDeviceCode(), fakeas.WithScopesSupported("read"))
	g := gatePolls(h)
	t.Cleanup(h.broker.Close)

	f := h.startDevice(t)

	if f.AuthorizeURL != "" || !f.Device() {
		t.Errorf("Start(device) = %+v, want a device prompt and no authorize URL", f.Prompt)
	}

	want := auth.Prompt{
		UserCode:                f.UserCode,
		VerificationURI:         h.as.Issuer + "/device",
		VerificationURIComplete: h.as.Issuer + "/device?user_code=" + f.UserCode,
	}

	if diff := cmp.Diff(want, f.Prompt); diff != "" || len(f.UserCode) != 9 {
		t.Errorf("prompt (-want +got):\n%s", diff)
	}

	reqs := h.as.DeviceAuthorizations()

	if len(reqs) != 1 || reqs[0].ClientID != testClientID || reqs[0].Scope != "read" || reqs[0].Resource != h.as.ResourceURL {
		t.Errorf("device authorization requests = %+v, want one by %s for scope read at the resource", reqs, testClientID)
	}

	for range 3 {
		if d := g.next(t); d != 5*time.Second {
			t.Errorf("poll interval = %s, want the server's 5s", d)
		}

		g.poll()
	}

	g.next(t)
	h.as.ApproveDevice(f.UserCode)
	g.poll()

	if res := h.waitDone(t, f.ID); res.State != auth.FlowDone || res.Err != nil {
		t.Fatalf("flow = %+v, want done", res)
	}

	if diff := cmp.Diff([]string{"authorization_pending", "authorization_pending", "authorization_pending", ""}, h.deviceGrants()); diff != "" {
		t.Errorf("device code grants (-want +got):\n%s", diff)
	}

	if tr := h.as.TokenRequests(); tr[len(tr)-1].Resource != h.as.ResourceURL {
		t.Errorf("device code grant resource = %q, want %q", tr[len(tr)-1].Resource, h.as.ResourceURL)
	}

	if diff := cmp.Diff([]string{"linear"}, h.changed.get()); diff != "" {
		t.Errorf("OnCredentialChanged (-want +got):\n%s", diff)
	}

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Errorf("GET resource = %d, %v; want 200 with the device flow's token", status, err)
	}
}

func TestDeviceFlowSlowDownLengthensEveryLaterInterval(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithDeviceCode())
	g := gatePolls(h)
	t.Cleanup(h.broker.Close)
	f := h.startDevice(t)

	var got []time.Duration

	for _, slow := range []bool{true, false, true, false, false} {
		got = append(got, g.next(t))

		if slow {
			h.as.SlowDownDevice(f.UserCode)
		}

		g.poll()
	}

	got = append(got, g.next(t))
	// The first wait precedes any answer; each slow_down adds 5s to the
	// wait after it and to every later one.
	want := []time.Duration{5 * time.Second, 10 * time.Second, 10 * time.Second, 15 * time.Second, 15 * time.Second, 15 * time.Second}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("poll intervals (-want +got):\n%s", diff)
	}
}

func TestDeviceFlowEnds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		act   func(*fakeas.Server, string) bool
		state auth.FlowState
		want  auth.Case
	}{
		{"denied", (*fakeas.Server).DenyDevice, auth.FlowFailed, auth.CaseAuthorizationDenied},
		{"expired", (*fakeas.Server).ExpireDevice, auth.FlowExpired, auth.CaseFlowExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBrokerHarness(t, fakeas.WithDeviceCode())
			g := gatePolls(h)
			t.Cleanup(h.broker.Close)
			f := h.startDevice(t)

			g.next(t)
			g.poll()
			g.next(t)
			tc.act(h.as, f.UserCode)
			g.poll()
			res := h.waitDone(t, f.ID)

			if res.State != tc.state || caseOf(res.Err) != tc.want {
				t.Errorf("flow = %s %v, want %s %s", res.State, res.Err, tc.state, tc.want)
			}

			if e, ok := auth.AsError(res.Err); !ok || e.Link != wantAuthURL {
				t.Errorf("flow error %v, want one carrying the auth link", res.Err)
			}

			if got := h.changed.get(); len(got) != 0 {
				t.Errorf("OnCredentialChanged = %v, want none", got)
			}
		})
	}
}

// TestDeviceFlowExpiresWithItsDeviceCode: a device code that expires
// before FlowTTL ends the flow then, as flow_expired, without polling past
// it.
func TestDeviceFlowExpiresWithItsDeviceCode(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithDeviceCode(), fakeas.WithDeviceTiming(60, 0))
	g := gatePolls(h)
	t.Cleanup(h.broker.Close)
	f := h.startDevice(t)

	if want := h.clock.Now().Add(time.Minute); !f.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %s, want the device code's expiry %s", f.ExpiresAt, want)
	}

	if d := g.next(t); d != 5*time.Second {
		t.Errorf("poll interval = %s, want RFC 8628's default 5s when the server names none", d)
	}

	g.poll()
	g.next(t)
	h.clock.Advance(time.Minute)
	g.poll()

	if res := h.waitDone(t, f.ID); res.State != auth.FlowExpired || caseOf(res.Err) != auth.CaseFlowExpired {
		t.Errorf("flow = %s %v, want expired flow_expired", res.State, res.Err)
	}

	if got := h.deviceGrants(); len(got) != 1 {
		t.Errorf("device code grants = %v, want the one before the expiry", got)
	}
}

// TestDeviceFlowRevokedWhilePollingStoresNothing is scenario 5: polling
// stops, and no token is stored after the revoke.
func TestDeviceFlowRevokedWhilePollingStoresNothing(t *testing.T) {
	t.Run("between polls", func(t *testing.T) {
		h := newBrokerHarness(t, fakeas.WithDeviceCode())
		g := gatePolls(h)
		t.Cleanup(h.broker.Close)
		f := h.startDevice(t)

		g.next(t)
		g.poll()
		g.next(t)

		if _, err := h.broker.Revoke(t.Context(), "linear"); err != nil {
			t.Fatalf("Revoke: %v", err)
		}

		if res := h.waitDone(t, f.ID); caseOf(res.Err) != auth.CaseFlowCancelled {
			t.Errorf("flow = %s %v, want flow_cancelled", res.State, res.Err)
		}

		h.as.ApproveDevice(f.UserCode)
		h.broker.Close() // returns once the poller has

		if got := h.deviceGrants(); len(got) != 1 {
			t.Errorf("device code grants = %v, want only the one before the revoke", got)
		}

		if got := h.changed.get(); len(got) != 0 {
			t.Errorf("OnCredentialChanged = %v, want none", got)
		}
	})

	t.Run("while the token is on its way", func(t *testing.T) {
		h := newBrokerHarness(t, fakeas.WithDeviceCode())
		// The revoke lands once the token response has arrived, before the
		// poller stores it.
		h.opts.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp, err := http.DefaultTransport.RoundTrip(r)

			if err != nil || !strings.HasSuffix(r.URL.Path, "/token") {
				return resp, err
			}

			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(body))

			if bytes.Contains(body, []byte("access_token")) {
				if _, err := h.broker.Revoke(context.WithoutCancel(r.Context()), "linear"); err != nil {
					t.Errorf("Revoke: %v", err)
				}
			}

			return resp, nil
		})}
		h.broker = auth.NewBroker(h.opts)
		g := gatePolls(h)
		t.Cleanup(h.broker.Close)
		f := h.startDevice(t)

		h.as.ApproveDevice(f.UserCode)
		g.next(t)
		g.poll()

		if res := h.waitDone(t, f.ID); caseOf(res.Err) != auth.CaseFlowCancelled {
			t.Errorf("flow = %s %v, want flow_cancelled", res.State, res.Err)
		}

		h.broker.Close()

		if diff := cmp.Diff([]string{""}, h.deviceGrants()); diff != "" {
			t.Errorf("device code grants (-want +got):\n%s", diff)
		}

		if got := h.changed.get(); len(got) != 0 {
			t.Errorf("OnCredentialChanged = %v, want none: the token came after the revoke", got)
		}

		if _, err := h.get(t); caseOf(err) != auth.CaseNoCredential {
			t.Errorf("GET resource = %v, want no_credential: nothing was stored", err)
		}
	})
}

func TestDeviceFlowOutdatedByAConfigChangeStopsPolling(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithDeviceCode())
	g := gatePolls(h)
	t.Cleanup(h.broker.Close)
	flows := h.flows()
	f, err := flows.Start(t.Context(), "linear", auth.StartOptions{Device: true})

	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	g.next(t)
	g.poll()
	g.next(t)
	h.conn.Auth.Scopes = []string{"admin"}
	flows.ConfigChanged()

	if res := h.waitDone(t, f.ID); caseOf(res.Err) != auth.CaseFlowCancelled {
		t.Errorf("flow = %s %v, want flow_cancelled", res.State, res.Err)
	}

	h.broker.Close()

	if got := h.deviceGrants(); len(got) != 1 {
		t.Errorf("device code grants = %v, want only the one before the change", got)
	}
}

func TestBrokerCloseStopsDevicePollers(t *testing.T) {
	transport := &http.Transport{}
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	h := newBrokerHarness(t, fakeas.WithDeviceCode())
	h.opts.Client = &http.Client{Transport: transport}
	h.broker = auth.NewBroker(h.opts) // with the real timer
	f := h.startDevice(t)

	h.broker.Close()
	transport.CloseIdleConnections()

	if res := h.waitDone(t, f.ID); caseOf(res.Err) != auth.CaseFlowCancelled {
		t.Errorf("flow = %s %v, want flow_cancelled", res.State, res.Err)
	}

	if _, err := h.flows().Start(t.Context(), "linear", auth.StartOptions{Device: true}); caseOf(err) != auth.CaseFlowCancelled {
		t.Errorf("Start after Close = %v, want flow_cancelled", err)
	}

	transport.CloseIdleConnections()
}

func TestDeviceFlowNeedsADeviceEndpoint(t *testing.T) {
	h := newBrokerHarness(t)
	t.Cleanup(h.broker.Close)

	_, err := h.flows().Start(t.Context(), "linear", auth.StartOptions{Device: true})

	if e, ok := auth.AsError(err); !ok || e.Case != auth.CaseDeviceUnsupported || e.Kind != auth.KindConfig {
		t.Fatalf("Start(device) = %v, want config device_unsupported", err)
	}

	if got := auth.HTTPStatus(err); got != http.StatusConflict {
		t.Errorf("HTTPStatus = %d, want 409", got)
	}

	if n := len(h.as.DeviceAuthorizations()); n != 0 {
		t.Errorf("device authorization requests = %d, want none", n)
	}

	// The config names the endpoint the metadata leaves out.
	h.conn.Auth.DeviceAuthorizationEndpoint = h.as.DeviceAuthorizationEndpoint
	g := gatePolls(h)
	f := h.startDevice(t)
	h.as.ApproveDevice(f.UserCode)
	g.next(t)
	g.poll()

	if res := h.waitDone(t, f.ID); res.State != auth.FlowDone {
		t.Errorf("flow = %s %v, want done through the config's endpoint", res.State, res.Err)
	}
}

func TestGrantDeviceCodeSelectsTheDeviceFlow(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithDeviceCode())
	t.Cleanup(h.broker.Close)
	gatePolls(h)
	h.conn.Auth.Grant = config.GrantDeviceCode

	f, err := h.flows().Start(t.Context(), "linear", auth.StartOptions{})

	if err != nil || !f.Device() || f.AuthorizeURL != "" {
		t.Fatalf("Start = %+v, %v; want a device code flow", f, err)
	}

	if n := len(h.as.Authorizations()); n != 0 {
		t.Errorf("authorize requests = %d, want none", n)
	}
}

// TestDeviceFlowIgnoresPKCE: PKCE is the authorization code's rule; a
// discovered connector whose server advertises no S256 can still use the
// device code.
func TestDeviceFlowIgnoresPKCE(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithDeviceCode(), fakeas.WithoutPKCEAdvertised())
	t.Cleanup(h.broker.Close)
	gatePolls(h)
	h.conn.Auth.ClientID = ""

	if _, err := h.flows().Start(t.Context(), "linear", auth.StartOptions{}); caseOf(err) != auth.CasePKCEUnsupported {
		t.Errorf("Start(authorization code) = %v, want pkce_unsupported", err)
	}

	if _, err := h.flows().Start(t.Context(), "linear", auth.StartOptions{Device: true}); err != nil {
		t.Errorf("Start(device) = %v, want started", err)
	}
}

func TestDeviceFlowRefusesAVerificationURIThatIsNotHTTPS(t *testing.T) {
	for _, u := range []string{"http://phish.example/device", "javascript:alert(1)"} {
		t.Run(u, func(t *testing.T) {
			h := newBrokerHarness(t, fakeas.WithDeviceCode(), fakeas.WithDeviceVerificationURI(u))
			t.Cleanup(h.broker.Close)

			_, err := h.flows().Start(t.Context(), "linear", auth.StartOptions{Device: true})

			if e, ok := auth.AsError(err); !ok || e.Case != auth.CaseMalformedResponse || !strings.Contains(e.Detail, "verification_uri") {
				t.Errorf("Start(device) = %v, want malformed_response about the verification_uri", err)
			}
		})
	}
}

// TestDeviceFlowRegistersAClientForTheDeviceGrant: a DCR client is
// registered for the device code when the server lists it, and a stored
// client registered without it is not reused for a device flow.
func TestDeviceFlowRegistersAClientForTheDeviceGrant(t *testing.T) {
	h := newBrokerHarness(t)
	t.Cleanup(h.broker.Close)
	g := gatePolls(h)
	h.conn.Auth.ClientID = ""
	h.conn.Auth.DeviceAuthorizationEndpoint = h.as.DeviceAuthorizationEndpoint

	// An authorization code client, registered when the server did not
	// list the device code.
	h.start(t)

	if got := h.as.Clients(); len(got) != 2 || slices.Contains(got[1].GrantTypes, fakeas.GrantDeviceCode) {
		t.Fatalf("clients = %+v, want a DCR client without the device grant", got)
	}

	f := h.startDevice(t)
	got := h.as.Clients()

	if len(got) != 3 || !slices.Contains(got[2].GrantTypes, fakeas.GrantDeviceCode) {
		t.Fatalf("clients = %+v, want a new DCR client with the device grant", got)
	}

	h.as.ApproveDevice(f.UserCode)
	g.next(t)
	g.poll()

	if res := h.waitDone(t, f.ID); res.State != auth.FlowDone {
		t.Fatalf("flow = %s %v, want done", res.State, res.Err)
	}

	// The device client serves both kinds from now on.
	h.start(t)
	h.startDevice(t)

	if n := len(h.as.Clients()); n != 3 {
		t.Errorf("clients = %d, want the device client reused", n)
	}
}

func TestDCRListsTheDeviceGrantWhenTheServerDoes(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithDeviceCode())
	t.Cleanup(h.broker.Close)
	h.conn.Auth.ClientID = ""

	h.start(t)
	got := h.as.Clients()
	want := []string{"authorization_code", "refresh_token", fakeas.GrantDeviceCode}

	if len(got) != 2 || !cmp.Equal(got[1].GrantTypes, want) {
		t.Errorf("DCR client = %+v, want grant_types %v", got, want)
	}
}

func TestStartPageShowsTheDeviceCode(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithDeviceCode())
	t.Cleanup(h.broker.Close)
	gatePolls(h)
	h.conn.Auth.Grant = config.GrantDeviceCode
	mux := h.pagesMux()

	_, nonce := confirm(t, mux, "linear")
	rec := post(t, mux, "linear", nonce)
	reqs := h.as.DeviceAuthorizations()

	if len(reqs) != 1 {
		t.Fatalf("device authorization requests = %+v, want one", reqs)
	}

	body := rec.Body.String()
	link := h.as.Issuer + "/device?user_code=" + reqs[0].UserCode

	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" ||
		!strings.Contains(body, reqs[0].UserCode) || !strings.Contains(body, `href="`+link+`"`) {
		t.Errorf("POST /auth/linear = %d %q, want 200 with the user code and a link to %s", rec.Code, body, link)
	}

	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}

	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("CSP = %q, want frame-ancestors 'none'", rec.Header().Get("Content-Security-Policy"))
	}
}
