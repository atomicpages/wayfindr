package auth_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

const (
	svcClientID = "svc"
	svcSecret   = "s3cret"
)

// newUnattendedHarness is a broker harness whose connector "linear" uses
// the client_credentials grant as the confidential client svc.
func newUnattendedHarness(t *testing.T, opts ...fakeas.Option) *brokerHarness {
	t.Helper()
	h := newBrokerHarness(t, append([]fakeas.Option{fakeas.WithClient(svcClientID, svcSecret)}, opts...)...)
	h.conn.Auth = &config.Auth{
		Type: config.AuthTypeOAuth, Grant: config.GrantClientCredentials,
		ClientID: svcClientID, ClientSecret: svcSecret, ValidateIssuer: true, ResourceIndicator: true,
	}

	return h
}

// ccGrants returns the client_credentials grants the fake saw:
// each one's scope, and its error after a colon when it failed.
func ccGrants(as *fakeas.Server) []string {
	var out []string

	for _, tr := range as.TokenRequests() {
		if tr.GrantType != fakeas.GrantClientCredentials {
			continue
		}

		s := tr.Scope

		if tr.Error != "" {
			s += ":" + tr.Error
		}

		out = append(out, s)
	}

	return out
}

// mustGet GETs the resource through the broker and wants 200.
func (h *brokerHarness) mustGet(t *testing.T) {
	t.Helper()

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Fatalf("get = %d, %v; want 200", status, err)
	}
}

// wantConfigError wants err to be the auth config error c, which carries
// no auth link, as upstream reads it, and returns its provider code.
func wantConfigError(t *testing.T, err error, c auth.Case) string {
	t.Helper()
	e, ok := auth.AsError(err)

	if !ok || e.Kind != auth.KindConfig || e.Case != c {
		t.Fatalf("err = %v, want auth config error %s", err, c)
	}

	var na needsAuther
	var ac authConfiger

	if errors.As(err, &na) || !errors.As(err, &ac) || !ac.AuthConfig() || e.Link != "" {
		t.Errorf("err = %v reads as needs auth or carries a link; want auth_error", err)
	}

	return e.ProviderCode
}

// Scenario 1, the first connect: the grant runs without a user, and
// nothing is stored.
func TestUnattendedFirstConnectStoresNothing(t *testing.T) {
	h := newUnattendedHarness(t, fakeas.WithUnattendedRefreshTokens())
	h.mustGet(t)
	h.mustGet(t)

	reqs := h.as.TokenRequests()

	if len(reqs) != 1 || reqs[0].GrantType != fakeas.GrantClientCredentials ||
		reqs[0].ClientID != svcClientID || reqs[0].AuthMethod != "client_secret_basic" || reqs[0].Resource != h.as.ResourceURL {
		t.Errorf("token requests = %+v, want one client_credentials grant as svc with basic auth and the resource", reqs)
	}

	names, err := h.store.ListCredentialConnectors(t.Context(), h.user)

	if err != nil || len(names) != 0 {
		t.Errorf("credentials = %q, %v; want none stored", names, err)
	}

	if got := h.changed.get(); len(got) != 0 {
		t.Errorf("OnCredentialChanged = %q, want none: an unattended token is not a credential", got)
	}

	if health, err := h.broker.Health(t.Context(), h.conn); err != nil || health != (auth.Health{}) {
		t.Errorf("Health = %+v, %v; want zero for an unattended grant", health, err)
	}
}

// A token near expiry is replaced by a new grant, never a refresh, even
// when the server sent a refresh token.
func TestUnattendedExpiryRegrants(t *testing.T) {
	h := newUnattendedHarness(t, fakeas.WithUnattendedRefreshTokens())
	h.mustGet(t)
	h.clock.Advance(2 * time.Hour)
	h.mustGet(t)

	if diff := cmp.Diff([]string{"", ""}, ccGrants(h.as)); diff != "" {
		t.Errorf("client_credentials grants (-want +got):\n%s", diff)
	}

	if got := h.refreshes(); len(got) != 0 {
		t.Errorf("refreshes = %q, want none", got)
	}
}

// Scenario 7's 401 half: a rejected token gets one new grant and one
// replay; a fresh token rejected too is grant_rejected, not needs_auth.
func TestUnattended401RegrantsOnceThenGrantRejected(t *testing.T) {
	h := newUnattendedHarness(t)
	h.mustGet(t)

	h.as.Revoke() // the held token dies; a new one works
	h.mustGet(t)

	if got := len(ccGrants(h.as)); got != 2 {
		t.Fatalf("grants after a revoked token = %d, want 2", got)
	}

	h.as.RefuseAllTokens(true)
	_, err := h.get(t)
	wantConfigError(t, err, auth.CaseGrantRejected)

	if got := len(ccGrants(h.as)); got != 3 {
		t.Errorf("grants after every token is refused = %d, want 3: one more, then no loop", got)
	}
}

// Scenario 7's 403 half: insufficient_scope runs the grant once with the
// union of the scopes and replays; when the provider withholds a scope,
// the replay's 403 is scope_denied, and a later 403 asking for nothing new
// fails at once, without a grant.
func TestUnattendedStepUp(t *testing.T) {
	t.Run("granted", func(t *testing.T) {
		h := newUnattendedHarness(t, fakeas.WithRequiredScopes("read", "write"))
		h.conn.Auth.Scopes = []string{"read"}
		h.mustGet(t)

		if diff := cmp.Diff([]string{"read", "read write"}, ccGrants(h.as)); diff != "" {
			t.Errorf("grants (-want +got):\n%s", diff)
		}

		h.mustGet(t)

		if got := len(ccGrants(h.as)); got != 2 {
			t.Errorf("grants after a second call = %d, want 2: the larger token is held", got)
		}
	})

	t.Run("withheld", func(t *testing.T) {
		h := newUnattendedHarness(t, fakeas.WithRequiredScopes("read", "write"), fakeas.WithWithheldScopes("write"))
		h.conn.Auth.Scopes = []string{"read"}

		_, err := h.get(t)
		wantConfigError(t, err, auth.CaseScopeDenied)

		if diff := cmp.Diff([]string{"read", "read write"}, ccGrants(h.as)); diff != "" {
			t.Errorf("grants (-want +got):\n%s", diff)
		}

		_, err = h.get(t)
		wantConfigError(t, err, auth.CaseScopeDenied)

		if got := len(ccGrants(h.as)); got != 2 {
			t.Errorf("grants after a second denied call = %d, want 2: nothing new to ask for", got)
		}
	})
}

// The grant's scopes: config, else the challenge's, else none; never the
// protected resource metadata's scopes_supported.
func TestUnattendedScopeChoice(t *testing.T) {
	h := newUnattendedHarness(t, fakeas.WithScopesSupported("everything"), fakeas.WithChallengeScope("mcp"))
	h.mustGet(t)

	h.as.Revoke() // a 401 brings the challenge, and its scope
	h.mustGet(t)

	if diff := cmp.Diff([]string{"", "mcp"}, ccGrants(h.as)); diff != "" {
		t.Errorf("grants (-want +got):\n%s", diff)
	}
}

// A refused grant is grant_rejected with the provider code, a config error
// with no auth link; a rejected client stays client_rejected.
func TestUnattendedRefusals(t *testing.T) {
	for _, tt := range []struct {
		code string
		want auth.Case
	}{
		{"invalid_grant", auth.CaseGrantRejected},
		{"invalid_scope", auth.CaseGrantRejected},
		{"unauthorized_client", auth.CaseGrantRejected},
		{"unsupported_grant_type", auth.CaseGrantRejected},
		{"invalid_client", auth.CaseClientRejected},
	} {
		t.Run(tt.code, func(t *testing.T) {
			h := newUnattendedHarness(t)
			h.as.FailTokenRequests(tt.code)

			_, err := h.get(t)
			if code := wantConfigError(t, err, tt.want); code != tt.code {
				t.Errorf("provider code = %q, want %q", code, tt.code)
			}

			if got := len(ccGrants(h.as)); got != 1 {
				t.Errorf("grants = %d, want 1: a refusal is final", got)
			}
		})
	}

	t.Run("a wrong secret", func(t *testing.T) {
		h := newUnattendedHarness(t)
		h.conn.Auth.ClientSecret = "wrong"

		_, err := h.get(t)
		wantConfigError(t, err, auth.CaseClientRejected)
	})
}

func TestUnattendedConcurrentCallsShareOneGrant(t *testing.T) {
	h := newUnattendedHarness(t)
	client := h.client()

	const calls = 50
	var wg sync.WaitGroup
	errs := make(chan error, calls)

	for range calls {
		wg.Go(func() {
			resp, err := send(t, client, http.MethodGet, h.as.ResourceURL, "")

			if err != nil {
				errs <- err

				return
			}

			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				errs <- errors.New(resp.Status)
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("call failed: %v", err)
	}

	if got := len(ccGrants(h.as)); got != 1 {
		t.Errorf("grants = %d, want 1", got)
	}
}

// Revoke forgets the in-memory token and reports a change, so the
// connector reconnects and the grant runs again; it does so after a
// refused grant too, which is how a user retries one. It also deletes a
// row left from an auth flow (scenario 1).
func TestUnattendedRevokeRegrants(t *testing.T) {
	h := newUnattendedHarness(t)
	h.mustGet(t)

	if found, err := h.broker.Revoke(t.Context(), "linear"); err != nil || !found {
		t.Fatalf("Revoke = %v, %v; want found", found, err)
	}

	if diff := cmp.Diff([]string{"linear"}, h.changed.get()); diff != "" {
		t.Errorf("OnCredentialChanged (-want +got):\n%s", diff)
	}

	h.mustGet(t)

	if got := len(ccGrants(h.as)); got != 2 {
		t.Errorf("grants after revoke = %d, want 2", got)
	}

	h.as.FailTokenRequests("invalid_grant")
	h.clock.Advance(2 * time.Hour)

	if _, err := h.get(t); err == nil {
		t.Fatal("get with the grant refused succeeded")
	}

	if found, err := h.broker.Revoke(t.Context(), "linear"); err != nil || !found {
		t.Errorf("Revoke after a refused grant = %v, %v; want found, to retry it", found, err)
	}

	// A row left by an auth flow before the grant was switched by hand.
	if err := auth.SeedToken(t.Context(), h.broker, "linear", h.as.ResourceURL, h.as.TokenEndpoint, "old", "", time.Time{}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if health, err := h.broker.Health(t.Context(), h.conn); err != nil || health != (auth.Health{}) {
		t.Errorf("Health with a stale row = %+v, %v; want zero", health, err)
	}

	if found, err := h.broker.Revoke(t.Context(), "linear"); err != nil || !found {
		t.Errorf("Revoke of the stale row = %v, %v; want found", found, err)
	}

	if names, err := h.store.ListCredentialConnectors(t.Context(), h.user); err != nil || len(names) != 0 {
		t.Errorf("credentials after revoke = %q, %v; want none", names, err)
	}
}

// Scenario 2: two connectors on one authorization server each hold their
// own token.
func TestUnattendedConnectorsKeepSeparateTokens(t *testing.T) {
	h := newUnattendedHarness(t, fakeas.WithClient("svc2", "other"))
	second := h.conn
	second.Name = "second"
	auth2 := *h.conn.Auth
	auth2.ClientID, auth2.ClientSecret = "svc2", "other"
	second.Auth = &auth2
	client2 := &http.Client{Transport: h.broker.Transport(second, http.DefaultTransport)}

	for range 2 {
		h.mustGet(t)

		resp, err := send(t, client2, http.MethodGet, h.as.ResourceURL, "")

		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("second connector get = %v, %v; want 200", resp, err)
		}

		_ = resp.Body.Close()
	}

	var clients []string

	for _, tr := range h.as.TokenRequests() {
		clients = append(clients, tr.ClientID)
	}

	if diff := cmp.Diff([]string{svcClientID, "svc2"}, clients); diff != "" {
		t.Errorf("token requests' clients (-want +got):\n%s", diff)
	}
}

// A config change makes the held token unusable: it was got as another
// client.
func TestUnattendedConfigChangeRegrants(t *testing.T) {
	h := newUnattendedHarness(t)
	h.mustGet(t)
	h.conn.Auth.Scopes = []string{"more"}
	h.mustGet(t)

	if diff := cmp.Diff([]string{"", "more"}, ccGrants(h.as)); diff != "" {
		t.Errorf("grants (-want +got):\n%s", diff)
	}
}

func TestUnattendedHasNoAuthFlow(t *testing.T) {
	h := newUnattendedHarness(t)

	for _, opts := range []auth.StartOptions{{}, {Device: true}} {
		_, err := h.flows().Start(t.Context(), "linear", opts)

		if e, ok := auth.AsError(err); !ok || e.Kind != auth.KindRequest || e.Case != auth.CaseNotInteractive {
			t.Errorf("Start(%+v) = %v, want not_interactive", opts, err)
		}

		if got := auth.HTTPStatus(err); got != http.StatusNotFound {
			t.Errorf("HTTPStatus = %d, want 404", got)
		}
	}

	// The auth link's page says so at once, rather than offering to
	// continue.
	rec, nonce := confirm(t, h.pagesMux(), "linear")

	if rec.Code != http.StatusNotFound || nonce != "" || !strings.Contains(rec.Body.String(), "nothing to sign in to") {
		t.Errorf("GET /auth/linear = %d %q, want 404 with the not_interactive advice", rec.Code, rec.Body.String())
	}
}
