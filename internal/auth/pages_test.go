package auth_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/auth"
)

// pagesMux serves the auth pages as httpd routes them, over the harness's
// flows, with "linear" as the only configured connector.
func (h *brokerHarness) pagesMux() *http.ServeMux {
	p := auth.NewPages(h.flows())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/{connector}", p.Confirm)
	mux.HandleFunc("POST /auth/{connector}", p.Start)

	return mux
}

var nonceField = regexp.MustCompile(`name="nonce" value="([^"]+)"`)

// confirm GETs the confirmation page and returns the response and the
// form's nonce.
func confirm(t *testing.T, mux http.Handler, connector string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/"+connector, nil))
	m := nonceField.FindStringSubmatch(rec.Body.String())

	if m == nil {
		return rec, ""
	}

	return rec, m[1]
}

// post submits the confirmation form with nonce.
func post(t *testing.T, mux http.Handler, connector, nonce string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/"+connector,
		strings.NewReader(url.Values{"nonce": {nonce}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	return rec
}

func TestConfirmPageAsksBeforeStartingTheFlow(t *testing.T) {
	h := newBrokerHarness(t)
	mux := h.pagesMux()

	rec, nonce := confirm(t, mux, "linear")

	if rec.Code != http.StatusOK || nonce == "" || !strings.Contains(rec.Body.String(), "Continue") {
		t.Fatalf("GET /auth/linear = %d %q, want 200 with a Continue form", rec.Code, rec.Body.String())
	}

	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY: the button must not be framed", got)
	}

	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("CSP = %q, want frame-ancestors 'none'", rec.Header().Get("Content-Security-Policy"))
	}

	// Under no-referrer a browser posts the form with Origin: null, which
	// httpd's same-origin guard refuses (wayfindr-vngr).
	if got := rec.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("Referrer-Policy = %q, want same-origin so the Continue post carries its Origin", got)
	}

	if n := len(h.as.Authorizations()) + len(h.as.TokenRequests()); n != 0 {
		t.Errorf("the GET reached the authorization server %d times, want none", n)
	}

	got := post(t, mux, "linear", nonce)

	if loc := got.Header().Get("Location"); got.Code != http.StatusFound || !strings.HasPrefix(loc, h.as.AuthorizationEndpoint+"?") {
		t.Errorf("POST /auth/linear = %d to %q, want 302 to the authorize endpoint", got.Code, loc)
	}
}

func TestStartPageNeedsAFreshNonce(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		h := newBrokerHarness(t)

		if got := post(t, h.pagesMux(), "linear", ""); got.Code != http.StatusForbidden {
			t.Errorf("POST without a nonce = %d, want 403", got.Code)
		}
	})
	t.Run("made up", func(t *testing.T) {
		h := newBrokerHarness(t)

		if got := post(t, h.pagesMux(), "linear", "not-a-nonce"); got.Code != http.StatusForbidden {
			t.Errorf("POST with a made-up nonce = %d, want 403", got.Code)
		}
	})
	t.Run("used", func(t *testing.T) {
		h := newBrokerHarness(t)
		mux := h.pagesMux()
		_, nonce := confirm(t, mux, "linear")
		post(t, mux, "linear", nonce)

		if got := post(t, mux, "linear", nonce); got.Code != http.StatusForbidden {
			t.Errorf("POST with a used nonce = %d, want 403", got.Code)
		}
	})
	t.Run("expired", func(t *testing.T) {
		h := newBrokerHarness(t)
		mux := h.pagesMux()
		_, nonce := confirm(t, mux, "linear")
		h.clock.Advance(10*time.Minute + time.Second)

		if got := post(t, mux, "linear", nonce); got.Code != http.StatusForbidden {
			t.Errorf("POST with an expired nonce = %d, want 403", got.Code)
		}
	})
	t.Run("a refused post starts no flow", func(t *testing.T) {
		h := newBrokerHarness(t)
		post(t, h.pagesMux(), "linear", "x")

		if n := len(h.as.Authorizations()) + len(h.as.Clients()) - 1; n != 0 {
			t.Errorf("the authorization server saw %d authorizations or registrations, want none", n)
		}
	})
}

func TestPagesOfAnUnknownConnectorAre404(t *testing.T) {
	h := newBrokerHarness(t)
	mux := h.pagesMux()

	rec, _ := confirm(t, mux, "nope")

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /auth/nope = %d, want 404", rec.Code)
	}

	if got := post(t, mux, "nope", "x"); got.Code != http.StatusNotFound {
		t.Errorf("POST /auth/nope = %d, want 404", got.Code)
	}
}

func TestStartPageShowsWhyAFlowCannotStart(t *testing.T) {
	h := newBrokerHarness(t)
	h.conn.Auth.ClientID = ""
	h.conn.Auth.AuthorizationEndpoint = h.as.AuthorizationEndpoint
	h.conn.Auth.TokenEndpoint = h.as.TokenEndpoint
	mux := h.pagesMux()
	_, nonce := confirm(t, mux, "linear")

	got := post(t, mux, "linear", nonce)

	if got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), "PKCE") {
		t.Errorf("POST = %d %q, want 409 with the PKCE advice", got.Code, got.Body.String())
	}
}
