package auth_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// The tests in this file cover what ends a connector's credential, or an
// auth flow that would make one (wayfindr-141): Revoke, a config change
// (Flows.ConfigChanged and the callback's own check), and the token expiry
// the status reports.

// Revoke deletes the whole credentials row, the stored DCR client with the
// token, and reports the change, so the connector needs auth.
func TestRevokeDeletesTheCredentialAndReportsTheChange(t *testing.T) {
	h := newBrokerHarness(t)
	h.seedDCR(t, time.Hour)

	found, err := h.broker.Revoke(t.Context(), "linear")

	if err != nil || !found {
		t.Fatalf("Revoke = %v, %v; want true, nil", found, err)
	}

	if _, err := h.store.GetCredential(t.Context(), h.user, "linear"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetCredential after Revoke: err = %v, want ErrNotFound", err)
	}

	// One change for the seeded grant, one for the revoke.
	if diff := cmp.Diff([]string{"linear", "linear"}, h.changed.get()); diff != "" {
		t.Errorf("OnCredentialChanged calls (-want +got):\n%s", diff)
	}

	_, err = h.get(t)

	if caseOf(err) != auth.CaseNoCredential {
		t.Errorf("get after Revoke = %v, want needs_auth no_credential", err)
	}
}

func TestRevokeWithoutACredentialReportsNone(t *testing.T) {
	h := newBrokerHarness(t)

	found, err := h.broker.Revoke(t.Context(), "linear")

	if err != nil || found {
		t.Errorf("Revoke = %v, %v; want false, nil", found, err)
	}

	if got := h.changed.get(); len(got) != 0 {
		t.Errorf("OnCredentialChanged calls = %q, want none: nothing changed", got)
	}
}

// A revoke, and a config change that removes the connector or changes its
// url or [auth] table, end the connector's pending flows: the browser's
// late callback exchanges no code and stores no token, and its page says
// why. The daemon's reload sweeps them (ConfigChanged); the callback
// checks the running config itself too, so an edit it sees first ends the
// flow without the sweep. The connector has no credential yet (its
// client_id is in the config, so the flow stored no row): a revoke that
// finds no credential still cancels its flows.
func TestCancelledFlowsStoreNoToken(t *testing.T) {
	editAuth := func(h *brokerHarness) {
		a := *h.conn.Auth
		a.Scopes = []string{"read", "write"}
		h.conn.Auth = &a
	}
	tests := []struct {
		name   string
		cancel func(*brokerHarness, *testing.T)
	}{
		{"Revoke", func(h *brokerHarness, t *testing.T) {
			if _, err := h.broker.Revoke(t.Context(), "linear"); err != nil {
				t.Fatalf("Revoke: %v", err)
			}
		}},
		{"connector removed", func(h *brokerHarness, _ *testing.T) {
			h.removed = true
			h.flows().ConfigChanged()
		}},
		{"url changed", func(h *brokerHarness, _ *testing.T) {
			h.conn.URL += "/v2"
			h.flows().ConfigChanged()
		}},
		{"[auth] table changed", func(h *brokerHarness, _ *testing.T) {
			editAuth(h)
			h.flows().ConfigChanged()
		}},
		{"[auth] table changed, seen by the callback first", func(h *brokerHarness, _ *testing.T) { editAuth(h) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newBrokerHarness(t)
			f := h.start(t)
			cb := consent(t, f.AuthorizeURL)

			tt.cancel(h, t)
			status, _ := h.callback(t, cb)

			if status != http.StatusBadRequest {
				t.Errorf("late callback = %d, want 400", status)
			}

			got := h.result(t, f.ID)

			if got.State != auth.FlowFailed || caseOf(got.Err) != auth.CaseFlowCancelled {
				t.Errorf("WaitFlow = %+v, want failed flow_cancelled", got)
			}

			if n := len(h.codeExchanges()); n != 0 {
				t.Errorf("code exchanges = %d, want none", n)
			}

			if _, err := h.store.GetCredential(t.Context(), h.user, "linear"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetCredential after the late callback: err = %v, want ErrNotFound", err)
			}
		})
	}
}

// A revoke that lands while the callback is exchanging the code wins: the
// exchanged token is not stored after the revoke.
func TestRevokeDuringTheCodeExchangeStoresNoToken(t *testing.T) {
	h := newBrokerHarness(t)
	exchanging, release := make(chan struct{}), make(chan struct{})
	o := h.opts
	o.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && r.URL.String() == h.as.TokenEndpoint {
			close(exchanging)
			<-release
		}

		return http.DefaultTransport.RoundTrip(r)
	})}
	h.broker = auth.NewBroker(o)
	f := h.start(t)
	cb := consent(t, f.AuthorizeURL)

	called := make(chan int, 1)
	go func() {
		status, _ := h.callback(t, cb)
		called <- status
	}()
	<-exchanging
	_, err := h.broker.Revoke(t.Context(), "linear")
	close(release)

	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if status := <-called; status != http.StatusBadRequest {
		t.Errorf("callback = %d, want 400", status)
	}

	if got := h.result(t, f.ID); got.State != auth.FlowFailed || caseOf(got.Err) != auth.CaseFlowCancelled {
		t.Errorf("WaitFlow = %+v, want failed flow_cancelled", got)
	}

	if _, err := h.store.GetCredential(t.Context(), h.user, "linear"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetCredential after the callback: err = %v, want ErrNotFound", err)
	}

	if got := h.changed.get(); len(got) != 0 {
		t.Errorf("OnCredentialChanged calls = %q, want none", got)
	}
}

// A config change to anything but the url and the [auth] table, such as
// the timeout, leaves the connector's pending flow alone, and its callback
// completes it.
func TestConfigChangeOfOtherFieldsLeavesTheFlowPending(t *testing.T) {
	h := newBrokerHarness(t)
	f := h.start(t)

	h.conn.Timeout = config.Duration{Duration: 45 * time.Second}
	h.conn.Headers = map[string]config.Template{"X-Team": "core"}
	h.flows().ConfigChanged()

	if got := h.result(t, f.ID); got.State != auth.FlowPending {
		t.Errorf("Wait after ConfigChanged = %+v, want pending", got)
	}

	if status, page := h.callback(t, consent(t, f.AuthorizeURL)); status != http.StatusOK {
		t.Errorf("callback = %d %q, want 200", status, page)
	}
}

func TestHealth(t *testing.T) {
	h := newBrokerHarness(t)

	if got, err := h.broker.Health(t.Context(), h.conn); err != nil || got != (auth.Health{}) {
		t.Errorf("Health with no credential = %+v, %v; want zero, nil", got, err)
	}

	h.seed(t, "", time.Hour)

	want := auth.Health{AccessExpiry: time.Date(2026, 11, 4, 13, 0, 0, 0, time.UTC), HasRefresh: true}

	got, err := h.broker.Health(t.Context(), h.conn)

	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Health (-want +got):\n%s", diff)
	}
}

// A token stored without an expiry (the server sent no expires_in) has
// none to report.
func TestHealthOfATokenWithoutAnExpiry(t *testing.T) {
	h := newBrokerHarness(t)

	if err := auth.SeedToken(t.Context(), h.broker, "linear", h.as.ResourceURL, h.as.TokenEndpoint, "access", "", time.Time{}, ""); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	if got, err := h.broker.Health(t.Context(), h.conn); err != nil || got != (auth.Health{}) {
		t.Errorf("Health = %+v, %v; want zero, nil", got, err)
	}
}

// A token bound to the connector's old url is never sent to the new one,
// so the status shows no health for it (wayfindr-zgp.14).
func TestHealthOfATokenBoundToAnOldURL(t *testing.T) {
	h := newBrokerHarness(t)
	h.seed(t, "", time.Hour)
	h.conn.URL = config.Template(h.as.ResourceURL + "/moved")

	if got, err := h.broker.Health(t.Context(), h.conn); err != nil || got != (auth.Health{}) {
		t.Errorf("Health after a url change = %+v, %v; want zero, nil", got, err)
	}
}

// The tests below cover writes that race a revoke or a new grant
// (wayfindr-zgp.16): only a grant starts a credentials row, and a refresh
// stores its token only over the token it refreshed.

// gate holds the first request match accepts until release is closed,
// closing reached when that request arrives. Other requests pass.
type gate struct {
	reached, release chan struct{}
	once             sync.Once
}

func newGate() *gate { return &gate{reached: make(chan struct{}), release: make(chan struct{})} }

func (g *gate) transport(match func(*http.Request) bool, next http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if match(r) {
			g.once.Do(func() {
				close(g.reached)
				<-g.release
			})
		}

		return next.RoundTrip(r)
	})
}

// gateTokenEndpoint rebuilds the broker so its first token request waits
// for the returned gate.
func (h *brokerHarness) gateTokenEndpoint() *gate {
	g := newGate()
	o := h.opts
	o.Client = &http.Client{Transport: g.transport(func(r *http.Request) bool {
		return r.Method == http.MethodPost && r.URL.String() == h.as.TokenEndpoint
	}, http.DefaultTransport)}
	h.broker = auth.NewBroker(o)

	return g
}

// bearers records the bearer tokens of the requests it passes on.
type bearers struct {
	names
	next http.RoundTripper
}

func (b *bearers) RoundTrip(r *http.Request) (*http.Response, error) {
	b.add(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))

	return b.next.RoundTrip(r)
}

// getWith GETs the resource through client in the background. Any answer
// but a 200 is an error.
func (h *brokerHarness) getWith(client *http.Client) <-chan error {
	done := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.as.ResourceURL, nil)

		if err != nil {
			done <- err

			return
		}

		resp, err := client.Do(req)

		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("status %d", resp.StatusCode)
			}
		}

		done <- err
	}()

	return done
}

// A refresh that was spending the refresh token when the user revoked does
// not bring the credential back: its token is dropped, and the call fails
// needing auth.
func TestRevokeDuringARefreshLeavesNoCredential(t *testing.T) {
	h := newBrokerHarness(t)
	h.seed(t, "", 30*time.Second)
	g := h.gateTokenEndpoint()

	called := h.getWith(h.client())
	<-g.reached
	found, err := h.broker.Revoke(t.Context(), "linear")
	close(g.release)

	if err != nil || !found {
		t.Fatalf("Revoke = %v, %v; want true, nil", found, err)
	}

	err = <-called
	var na needsAuther

	if caseOf(err) != auth.CaseNoCredential || !errors.As(err, &na) {
		t.Fatalf("get during the revoke = %v, want needs_auth no_credential", err)
	}

	if link, _ := na.NeedsAuth(); link != wantAuthURL {
		t.Errorf("link = %q, want %q", link, wantAuthURL)
	}

	if _, err := h.store.GetCredential(t.Context(), h.user, "linear"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetCredential after the refresh: err = %v, want ErrNotFound", err)
	}

	if got, err := h.broker.Health(t.Context(), h.conn); err != nil || got != (auth.Health{}) {
		t.Errorf("Health = %+v, %v; want zero: there is no token", got, err)
	}
}

// A new grant stored while a refresh of the old one is in flight is the
// credential from then on: the refresh drops its token, and the call uses
// the new grant's.
func TestNewGrantDuringARefreshIsKept(t *testing.T) {
	h := newBrokerHarness(t)
	h.seed(t, "", 30*time.Second)
	g := h.gateTokenEndpoint()
	sent := &bearers{next: http.DefaultTransport}

	called := h.getWith(&http.Client{Transport: h.broker.Transport(h.conn, sent)})
	<-g.reached
	access, refresh := h.authorize(t, "")
	expiry := h.clock.Now().Add(2 * time.Hour)
	err := auth.SeedToken(t.Context(), h.broker, "linear", h.as.ResourceURL, h.as.TokenEndpoint, access, refresh, expiry, "")
	close(g.release)

	if err != nil {
		t.Fatalf("store the new grant: %v", err)
	}

	if err := <-called; err != nil {
		t.Fatalf("get during the new grant: %v", err)
	}

	if got, err := h.broker.Health(t.Context(), h.conn); err != nil || !got.AccessExpiry.Equal(expiry) {
		t.Errorf("Health = %+v, %v; want the new grant's expiry %v", got, err, expiry)
	}

	if diff := cmp.Diff([]string{access}, sent.get()); diff != "" {
		t.Errorf("bearer tokens sent (-want +got):\n%s", diff)
	}
}

// A step-up answered after the credential was revoked records no scopes:
// there is no credential to add them to, and none is started.
func TestRevokeDuringAStepUpLeavesNoCredential(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithRequiredScopes("read", "write"))
	h.seed(t, "read", time.Hour)
	g := newGate()
	next := g.transport(func(r *http.Request) bool { return r.URL.String() == h.as.ResourceURL }, http.DefaultTransport)

	called := h.getWith(&http.Client{Transport: h.broker.Transport(h.conn, next)})
	<-g.reached
	found, err := h.broker.Revoke(t.Context(), "linear")
	close(g.release)

	if err != nil || !found {
		t.Fatalf("Revoke = %v, %v; want true, nil", found, err)
	}

	if err := <-called; caseOf(err) != auth.CaseNoCredential {
		t.Errorf("get during the revoke = %v, want needs_auth no_credential", err)
	}

	if _, err := h.store.GetCredential(t.Context(), h.user, "linear"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetCredential after the step-up: err = %v, want ErrNotFound", err)
	}
}

// A refresh whose DCR client is rejected after the credential was revoked
// has no stored client to forget, and starts no row.
func TestRevokeDuringARejectedDCRRefreshLeavesNoCredential(t *testing.T) {
	h := newBrokerHarness(t)
	old := h.seedDCR(t, 30*time.Second)
	h.as.RevokeClient(old.ClientID)
	g := h.gateTokenEndpoint()

	called := h.getWith(h.client())
	<-g.reached
	found, err := h.broker.Revoke(t.Context(), "linear")
	close(g.release)

	if err != nil || !found {
		t.Fatalf("Revoke = %v, %v; want true, nil", found, err)
	}

	if err := <-called; caseOf(err) != auth.CaseNoCredential {
		t.Errorf("get during the revoke = %v, want needs_auth no_credential", err)
	}

	if _, err := h.store.GetCredential(t.Context(), h.user, "linear"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetCredential after the refresh: err = %v, want ErrNotFound", err)
	}
}

// A refresh whose DCR client is rejected after a new grant was stored
// keeps the client: the newer grant was issued to it, so it is not the
// expired registration the rejection suggests. The call uses the new
// grant's token.
func TestNewGrantDuringARejectedDCRRefreshKeepsTheClient(t *testing.T) {
	h := newBrokerHarness(t)
	reg := h.seedDCR(t, 30*time.Second)
	access, refresh := h.authorizeAs(t, reg, "")
	g := h.gateTokenEndpoint()
	sent := &bearers{next: http.DefaultTransport}
	h.as.FailTokenRequests("invalid_client")

	called := h.getWith(&http.Client{Transport: h.broker.Transport(h.conn, sent)})
	<-g.reached
	expiry := h.clock.Now().Add(2 * time.Hour)
	err := auth.SeedToken(t.Context(), h.broker, "linear", h.as.ResourceURL, h.as.TokenEndpoint, access, refresh, expiry, "")
	close(g.release)

	if err != nil {
		t.Fatalf("store the new grant: %v", err)
	}

	if err := <-called; err != nil {
		t.Fatalf("get during the new grant: %v", err)
	}

	if diff := cmp.Diff([]string{access}, sent.get()); diff != "" {
		t.Errorf("bearer tokens sent (-want +got):\n%s", diff)
	}

	cred, err := h.store.GetCredential(t.Context(), h.user, "linear")

	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}

	if cred.DCRClientID != reg.ClientID || len(cred.DCRClient) == 0 {
		t.Errorf("stored client = %q (%d sealed bytes), want %q kept", cred.DCRClientID, len(cred.DCRClient), reg.ClientID)
	}
}

// A flow is in the flow table from the start of Start: a revoke, or a
// config change, while it is still starting (here held in discovery)
// cancels it. Start fails flow_cancelled, and the authorization server
// sees no authorize request.
func TestFlowEndedWhileItStartsIsCancelled(t *testing.T) {
	tests := []struct {
		name   string
		cancel func(*brokerHarness, *testing.T)
	}{
		{"Revoke", func(h *brokerHarness, t *testing.T) {
			if _, err := h.broker.Revoke(t.Context(), "linear"); err != nil {
				t.Fatalf("Revoke: %v", err)
			}
		}},
		{"connector removed", func(h *brokerHarness, _ *testing.T) {
			h.removed = true
			h.flows().ConfigChanged()
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newBrokerHarness(t)
			g := newGate()
			o := h.opts
			o.Client = &http.Client{Transport: g.transport(func(r *http.Request) bool {
				return strings.Contains(r.URL.Path, "/.well-known/oauth-authorization-server")
			}, http.DefaultTransport)}
			h.broker = auth.NewBroker(o)
			flows := h.flows()
			started := make(chan error, 1)
			go func() {
				_, err := flows.Start(context.Background(), "linear", auth.StartOptions{})
				started <- err
			}()
			<-g.reached

			tt.cancel(h, t)
			close(g.release)

			if err := <-started; caseOf(err) != auth.CaseFlowCancelled {
				t.Errorf("Start = %v, want flow_cancelled", err)
			}

			if n := len(h.as.Authorizations()); n != 0 {
				t.Errorf("authorize requests = %d, want none", n)
			}
		})
	}
}
