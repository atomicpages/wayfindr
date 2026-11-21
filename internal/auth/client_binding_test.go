package auth_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// The tests in this file are about the client a grant is bound to: a token
// is never sent once the connector's client is not the one that got it.

const otherClientID = "wayfindr-other"

// bearerRequests counts the resource requests that carried a bearer token.
func bearerRequests(h *brokerHarness) int {
	n := 0

	for _, r := range h.as.ResourceRequests() {
		if r.Bearer {
			n++
		}
	}

	return n
}

// authorizedThroughFlow runs the auth flow to completion with the
// connector's current config.
func (h *brokerHarness) authorizedThroughFlow(t *testing.T) {
	t.Helper()
	f := h.start(t)

	if status, page := h.callback(t, consent(t, f.AuthorizeURL)); status != http.StatusOK {
		t.Fatalf("callback = %d %q, want 200", status, page)
	}
}

func (h *brokerHarness) storedClientID(t *testing.T) string {
	t.Helper()
	cred, err := h.store.GetCredential(t.Context(), h.user, "linear")

	if err != nil {
		t.Fatalf("get credential: %v", err)
	}

	return cred.ClientID
}

func TestGrantRecordsTheClientThatObtainedIt(t *testing.T) {
	t.Run("manual", func(t *testing.T) {
		h := newBrokerHarness(t)
		h.authorizedThroughFlow(t)

		if got := h.storedClientID(t); got != testClientID {
			t.Errorf("stored client_id = %q, want %q", got, testClientID)
		}
	})

	t.Run("dcr", func(t *testing.T) {
		h := newBrokerHarness(t)
		h.conn.Auth.ClientID = ""
		h.authorizedThroughFlow(t)

		cred, err := h.store.GetCredential(t.Context(), h.user, "linear")

		if err != nil {
			t.Fatalf("get credential: %v", err)
		}

		if cred.ClientID == "" || cred.ClientID != cred.DCRClientID {
			t.Errorf("stored client_id = %q, want the DCR client %q", cred.ClientID, cred.DCRClientID)
		}
	})
}

func TestRefreshKeepsOrFillsTheGrantsClient(t *testing.T) {
	t.Run("fills an unknown client", func(t *testing.T) {
		h := newBrokerHarness(t)
		h.seed(t, "", time.Second) // no client recorded; expiring, so the next use refreshes
		h.clock.Advance(time.Minute)

		if status, err := h.get(t); err != nil || status != http.StatusOK {
			t.Fatalf("get = %d, %v; want 200", status, err)
		}

		if diff := cmp.Diff([]string{""}, h.refreshes()); diff != "" {
			t.Errorf("refreshes (-want +got):\n%s", diff)
		}

		if got := h.storedClientID(t); got != testClientID {
			t.Errorf("stored client_id = %q, want %q, the client that refreshed", got, testClientID)
		}
	})

	t.Run("keeps the client", func(t *testing.T) {
		h := newBrokerHarness(t)
		h.authorizedThroughFlow(t)
		h.clock.Advance(24 * time.Hour)

		if status, err := h.get(t); err != nil || status != http.StatusOK {
			t.Fatalf("get = %d, %v; want 200", status, err)
		}

		if got := h.storedClientID(t); got != testClientID {
			t.Errorf("stored client_id = %q, want %q", got, testClientID)
		}
	})
}

func TestChangedClientIDNeedsAuthAndSendsNoToken(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithClient(otherClientID, "", defaultRedirect))
	h.authorizedThroughFlow(t)

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Fatalf("get before the change = %d, %v; want 200", status, err)
	}

	sent := bearerRequests(h)
	h.conn.Auth.ClientID = otherClientID
	_, err := h.get(t)

	if got := caseOf(err); got != auth.CaseClientChanged {
		t.Fatalf("get error = %v (case %q), want needs_auth client_changed", err, got)
	}

	var na needsAuther

	if !errors.As(err, &na) {
		t.Errorf("error %v is not needs_auth", err)
	}

	if got := bearerRequests(h); got != sent {
		t.Errorf("bearer requests = %d, want %d: the old client's token must not be sent", got, sent)
	}

	if got := h.refreshes(); len(got) != 0 {
		t.Errorf("refreshes = %q, want none", got)
	}
}

// TestSwitchBetweenManualAndDCR: switching a discovered connector to a
// manual client needs auth; dropping the manual client_id doesn't, here,
// since a discovered connector's client isn't checked (connector edit
// revokes the credential instead).
func TestSwitchBetweenManualAndDCR(t *testing.T) {
	t.Run("manual to dcr is not checked", func(t *testing.T) {
		h := newBrokerHarness(t)
		h.authorizedThroughFlow(t)
		h.conn.Auth.ClientID = ""

		if _, err := h.get(t); caseOf(err) == auth.CaseClientChanged {
			t.Fatalf("get error = %v, want no client_changed for a discovered connector", err)
		}
	})

	t.Run("dcr to manual", func(t *testing.T) {
		h := newBrokerHarness(t)
		h.conn.Auth.ClientID = ""
		h.authorizedThroughFlow(t)

		if status, err := h.get(t); err != nil || status != http.StatusOK {
			t.Fatalf("get before the change = %d, %v; want 200", status, err)
		}

		before := bearerRequests(h)
		h.conn.Auth.ClientID = testClientID

		if _, err := h.get(t); caseOf(err) != auth.CaseClientChanged {
			t.Fatalf("get error = %v, want client_changed", err)
		}

		if got := bearerRequests(h); got != before {
			t.Errorf("bearer requests = %d, want %d", got, before)
		}
	})
}

func TestGrantWithoutRecordedClientIsAccepted(t *testing.T) {
	h := newBrokerHarness(t)
	h.seed(t, "", time.Hour) // SeedToken records no client, as a row from before the binding

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Errorf("get = %d, %v; want 200", status, err)
	}

	h.conn.Auth.ClientID = otherClientID

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Errorf("get with another client_id = %d, %v; want 200: nothing is known to differ", status, err)
	}
}

func TestChangedClientSecretKeepsTheToken(t *testing.T) {
	h := newBrokerHarness(t)
	h.authorizedThroughFlow(t)
	h.conn.Auth.ClientSecret = config.Template("a-new-secret")

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Errorf("get = %d, %v; want 200: a new client_secret alone keeps the token", status, err)
	}
}
