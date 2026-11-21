package auth_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// unlistedPRM is a resource whose PRM sits at a path only its 401 challenge
// names, with the authorization server under a tenant path, so the legacy
// fallback at the resource's origin finds nothing that matches.
func unlistedPRM() []fakeas.Option {
	return []fakeas.Option{
		fakeas.WithChallengeResourceMetadata(), fakeas.WithPRMLocation(fakeas.PRMUnlisted),
		fakeas.WithIssuerPath("/tenant1"),
	}
}

// A refresh before the token expires has no challenge to read the PRM URL
// from. It refreshes where the grant was obtained all the same, after a
// restart too, because the grant remembers where its PRM was
// (wayfindr-q8y9). Before, discovery found no PRM, fell back to the legacy
// endpoints at the resource's origin, and the refresh failed as
// authorization_server_changed: a sign-in every access-token lifetime.
func TestLazyRefreshFindsAPRMThatOnlyTheChallengeNamed(t *testing.T) {
	h := newBrokerHarness(t, unlistedPRM()...)
	h.authorizedThroughFlow(t)

	// A restart: the challenge the flow probed is gone with the process.
	h.broker = auth.NewBroker(h.opts)
	h.clock.Advance(24 * time.Hour)

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Fatalf("get = %d, %v; want 200 after a refresh", status, err)
	}

	if got := h.refreshes(); len(got) != 1 || got[0] != "" {
		t.Errorf("refresh grants = %q, want one that succeeded", got)
	}
}

// A grant stored before the PRM URL was recorded has none: its refresh
// probes the resource for the challenge instead, and succeeds. It records
// what that discovery found, so a later refresh, after another restart,
// probes nothing.
func TestLazyRefreshOfAnOlderGrantProbesForThePRMOnce(t *testing.T) {
	h := newBrokerHarness(t, unlistedPRM()...)
	h.seed(t, "", time.Minute)
	h.broker = auth.NewBroker(h.opts)
	h.clock.Advance(time.Hour)

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Fatalf("get = %d, %v; want 200 after a refresh", status, err)
	}

	if got := h.refreshes(); len(got) != 1 || got[0] != "" {
		t.Errorf("refresh grants = %q, want one that succeeded", got)
	}

	before := h.probes()
	h.broker = auth.NewBroker(h.opts)
	h.clock.Advance(24 * time.Hour)

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Fatalf("second get = %d, %v; want 200 after a refresh", status, err)
	}

	if n := h.probes() - before; n != 0 {
		t.Errorf("the second refresh probed the resource %d times, want none", n)
	}
}

// A grant whose flow found no PRM, because authorization_server names the
// server, refreshes without probing the resource: there is no PRM to find.
func TestLazyRefreshWithAnAuthorizationServerDoesNotProbe(t *testing.T) {
	h := newBrokerHarness(t)
	h.conn.Auth.AuthorizationServer = h.as.Issuer
	h.authorizedThroughFlow(t)
	before := h.probes()
	h.broker = auth.NewBroker(h.opts)
	h.clock.Advance(24 * time.Hour)

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Fatalf("get = %d, %v; want 200 after a refresh", status, err)
	}

	if n := h.probes() - before; n != 0 {
		t.Errorf("the refresh probed the resource %d times, want none", n)
	}
}
