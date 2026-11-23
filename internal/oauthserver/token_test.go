package oauthserver_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/oauthserver"
	"github.com/atomicpages/wayfindr/internal/store"
)

func TestTokenExchange(t *testing.T) {
	h := newHarness(t)
	code := h.code(t, "chatgpt")
	h.clock.Advance(10 * time.Second)

	w := h.tokenRaw(h.exchangeForm(code).Encode(), "application/x-www-form-urlencoded; charset=utf-8", nil)

	headers := map[string]string{
		"Content-Type":  w.Header().Get("Content-Type"),
		"Cache-Control": w.Header().Get("Cache-Control"),
		"Pragma":        w.Header().Get("Pragma"),
	}
	if diff := cmp.Diff(map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store", "Pragma": "no-cache"}, headers); diff != "" {
		t.Errorf("headers (-want +got):\n%s", diff)
	}

	a := decodeToken(t, w)
	if a.Status != http.StatusOK || a.TokenType != "Bearer" || a.ExpiresIn != 3600 {
		t.Fatalf("answer %+v", a)
	}

	if !strings.HasPrefix(a.AccessToken, "wf_s_") || !strings.HasPrefix(a.RefreshToken, "wf_s_") || a.AccessToken == a.RefreshToken {
		t.Fatalf("tokens %q and %q", a.AccessToken, a.RefreshToken)
	}

	access, err := h.grants.GrantToken(t.Context(), clienttoken.Digest(a.AccessToken))
	if err != nil {
		t.Fatal(err)
	}

	expires := start.Add(10*time.Second + time.Hour)
	if access.Grant != "chatgpt" || access.Kind != store.OAuthAccess || access.ExpiresAt == nil || !access.ExpiresAt.Equal(expires) {
		t.Errorf("access token row %+v, want grant chatgpt expiring %v", access, expires)
	}

	refresh, err := h.grants.GrantToken(t.Context(), clienttoken.Digest(a.RefreshToken))
	if err != nil {
		t.Fatal(err)
	}

	if refresh.Grant != "chatgpt" || refresh.Kind != store.OAuthRefresh || refresh.ExpiresAt != nil {
		t.Errorf("refresh token row %+v", refresh)
	}

	// Without resource, which is optional at the token endpoint.
	form := h.exchangeForm(h.code(t, "chatgpt-2"))
	form.Del("resource")

	if a := h.token(t, form); a.Status != http.StatusOK {
		t.Errorf("exchange without resource: %d %s", a.Status, a.Error)
	}
}

func TestTokenExchangeRefusals(t *testing.T) {
	tests := []struct {
		name   string
		form   func(h *harness, f url.Values)
		before func(h *harness)
		status int
		want   string
	}{
		{name: "wrong verifier", form: func(_ *harness, f url.Values) { f.Set("code_verifier", strings.Repeat("a", 43)) }, want: "invalid_grant"},
		{name: "verifier too short", form: func(_ *harness, f url.Values) { f.Set("code_verifier", verifier[:42]) }, want: "invalid_grant"},
		{name: "verifier with a bad character", form: func(_ *harness, f url.Values) { f.Set("code_verifier", verifier[:42]+"+") }, want: "invalid_grant"},
		{name: "no verifier", form: func(_ *harness, f url.Values) { f.Del("code_verifier") }, want: "invalid_request"},
		{name: "wrong redirect_uri", form: func(_ *harness, f url.Values) { f.Set("redirect_uri", "https://chatgpt.example/cb?x=1") }, want: "invalid_grant"},
		{name: "no redirect_uri", form: func(_ *harness, f url.Values) { f.Del("redirect_uri") }, want: "invalid_request"},
		{name: "wrong resource", form: func(_ *harness, f url.Values) { f.Set("resource", "https://w.example/other") }, want: "invalid_target"},
		{name: "another allowed client", form: func(_ *harness, f url.Values) { f.Set("client_id", otherClient) }, want: "invalid_grant"},
		{name: "unlisted client", form: func(_ *harness, f url.Values) { f.Set("client_id", "https://evil.example/c.json") }, status: http.StatusUnauthorized, want: "invalid_client"},
		{name: "no client_id", form: func(_ *harness, f url.Values) { f.Del("client_id") }, want: "invalid_request"},
		{name: "unknown code", form: func(_ *harness, f url.Values) { f.Set("code", "not-a-code") }, want: "invalid_grant"},
		{name: "expired code", before: func(h *harness) { h.clock.Advance(oauthserver.CodeTTL) }, want: "invalid_grant"},
		{name: "public_url changed", before: func(h *harness) {
			h.setServer(func(s *config.Server) { s.PublicURL = "https://elsewhere.example" })
		}, form: func(_ *harness, f url.Values) { f.Del("resource") }, want: "invalid_grant"},
		{name: "suspended", before: func(h *harness) { h.grants.suspended = true }, want: "invalid_grant"},
		{name: "revoked meanwhile", before: func(h *harness) { _ = h.grants.RevokeGrant(context.Background(), "chatgpt") }, want: "invalid_grant"},
		{name: "unsupported grant_type", form: func(_ *harness, f url.Values) { f.Set("grant_type", "password") }, want: "unsupported_grant_type"},
		{name: "no grant_type", form: func(_ *harness, f url.Values) { f.Del("grant_type") }, want: "unsupported_grant_type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			code := h.code(t, "chatgpt")

			if tt.before != nil {
				tt.before(h)
			}

			form := h.exchangeForm(code)
			if tt.form != nil {
				tt.form(h, form)
			}

			a := h.token(t, form)

			status := tt.status
			if status == 0 {
				status = http.StatusBadRequest
			}

			if a.Status != status || a.Error != tt.want || a.ErrorDescription == "" || a.AccessToken != "" {
				t.Errorf("answer %+v, want %d %s", a, status, tt.want)
			}

			if strings.Contains(a.ErrorDescription, code) || strings.Contains(a.ErrorDescription, verifier) {
				t.Error("the error echoes the request")
			}
		})
	}
}

func TestTokenExchangeRefusesAShortVerifierThatMatches(t *testing.T) {
	// RFC 7636 §4.1: at least 43 characters, even when its S256 is the
	// challenge sent.
	short := strings.Repeat("v", 42)
	sum := sha256.Sum256([]byte(short))

	h := newHarness(t)
	q := h.query()
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))

	redirect, err := h.srv.Approve(t.Context(), h.claimedID(t, q), "chatgpt", []string{"github"})
	if err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}

	form := h.exchangeForm(u.Query().Get("code"))
	form.Set("code_verifier", short)

	if a := h.token(t, form); a.Error != "invalid_grant" {
		t.Errorf("answer %+v, want invalid_grant", a)
	}
}

func TestCodeReplay(t *testing.T) {
	t.Run("within PendingTTL revokes the grant", func(t *testing.T) {
		h := newHarness(t)
		code := h.code(t, "chatgpt")

		if a := h.token(t, h.exchangeForm(code)); a.Status != http.StatusOK {
			t.Fatalf("exchange: %+v", a)
		}

		h.clock.Advance(oauthserver.PendingTTL - time.Second)

		if a := h.token(t, h.exchangeForm(code)); a.Error != "invalid_grant" {
			t.Errorf("replay: %+v, want invalid_grant", a)
		}

		if diff := cmp.Diff([]string{"chatgpt"}, h.grants.Revoked()); diff != "" {
			t.Errorf("revoked (-want +got):\n%s", diff)
		}
	})

	t.Run("a failed first presentation spends it too", func(t *testing.T) {
		h := newHarness(t)
		code := h.code(t, "chatgpt")

		bad := h.exchangeForm(code)
		bad.Set("code_verifier", strings.Repeat("b", 43))

		if a := h.token(t, bad); a.Error != "invalid_grant" {
			t.Fatalf("bad verifier: %+v", a)
		}

		if a := h.token(t, h.exchangeForm(code)); a.Error != "invalid_grant" {
			t.Errorf("second presentation: %+v, want invalid_grant", a)
		}

		if h.grants.hasGrant("chatgpt") {
			t.Error("the grant survived the replay")
		}
	})

	t.Run("after PendingTTL is only unknown", func(t *testing.T) {
		h := newHarness(t)
		code := h.code(t, "chatgpt")

		if a := h.token(t, h.exchangeForm(code)); a.Status != http.StatusOK {
			t.Fatalf("exchange: %+v", a)
		}

		h.clock.Advance(oauthserver.PendingTTL)

		if a := h.token(t, h.exchangeForm(code)); a.Error != "invalid_grant" {
			t.Errorf("replay: %+v, want invalid_grant", a)
		}

		if got := h.grants.Revoked(); len(got) != 0 {
			t.Errorf("revoked %v, want none", got)
		}
	})

	t.Run("a grant re-created under the name is left alone", func(t *testing.T) {
		h := newHarness(t)
		code := h.code(t, "chatgpt")

		if a := h.token(t, h.exchangeForm(code)); a.Status != http.StatusOK {
			t.Fatalf("exchange: %+v", a)
		}

		if err := h.grants.RevokeGrant(t.Context(), "chatgpt"); err != nil {
			t.Fatal(err)
		}

		h.clock.Advance(time.Second)

		if err := h.grants.CreateGrant(t.Context(), store.OAuthGrant{Name: "chatgpt", CreatedAt: h.clock.Now()}); err != nil {
			t.Fatal(err)
		}

		if a := h.token(t, h.exchangeForm(code)); a.Error != "invalid_grant" {
			t.Errorf("replay: %+v, want invalid_grant", a)
		}

		if !h.grants.hasGrant("chatgpt") {
			t.Error("the re-created grant was revoked")
		}
	})
}

func TestRefresh(t *testing.T) {
	h := newHarness(t)
	access, refresh := h.connect(t)
	h.clock.Advance(time.Hour)

	a := h.token(t, h.refreshForm(refresh))
	if a.Status != http.StatusOK || a.TokenType != "Bearer" || a.ExpiresIn != 3600 {
		t.Fatalf("refresh: %+v", a)
	}

	if a.AccessToken == access || a.RefreshToken == refresh || !strings.HasPrefix(a.RefreshToken, "wf_s_") {
		t.Errorf("rotation returned old tokens")
	}

	if _, err := h.grants.GrantToken(t.Context(), clienttoken.Digest(access)); err == nil {
		t.Error("the old access token is still stored")
	}

	spent, err := h.grants.GrantToken(t.Context(), clienttoken.Digest(refresh))
	if err != nil || spent.UsedAt == nil {
		t.Errorf("old refresh token %+v, %v: want spent", spent, err)
	}

	// The new refresh token works in turn.
	if b := h.token(t, h.refreshForm(a.RefreshToken)); b.Status != http.StatusOK {
		t.Errorf("second refresh: %+v", b)
	}
}

func TestRefreshReuse(t *testing.T) {
	t.Run("within RefreshGrace is refused, not revoked", func(t *testing.T) {
		h := newHarness(t)
		_, refresh := h.connect(t)

		if a := h.token(t, h.refreshForm(refresh)); a.Status != http.StatusOK {
			t.Fatalf("refresh: %+v", a)
		}

		h.clock.Advance(oauthserver.RefreshGrace - time.Second)

		if a := h.token(t, h.refreshForm(refresh)); a.Error != "invalid_grant" {
			t.Errorf("reuse: %+v, want invalid_grant", a)
		}

		if got := h.grants.Revoked(); len(got) != 0 {
			t.Errorf("revoked %v, want none", got)
		}
	})

	t.Run("after RefreshGrace revokes", func(t *testing.T) {
		h := newHarness(t)
		_, refresh := h.connect(t)

		a := h.token(t, h.refreshForm(refresh))
		if a.Status != http.StatusOK {
			t.Fatalf("refresh: %+v", a)
		}

		h.clock.Advance(oauthserver.RefreshGrace)

		if b := h.token(t, h.refreshForm(refresh)); b.Error != "invalid_grant" {
			t.Errorf("reuse: %+v, want invalid_grant", b)
		}

		if diff := cmp.Diff([]string{"chatgpt"}, h.grants.Revoked()); diff != "" {
			t.Errorf("revoked (-want +got):\n%s", diff)
		}

		// The successor is gone with the grant.
		if b := h.token(t, h.refreshForm(a.RefreshToken)); b.Error != "invalid_grant" {
			t.Errorf("successor after revoke: %+v, want invalid_grant", b)
		}
	})

	t.Run("a grant re-created under the name since is left alone", func(t *testing.T) {
		h := newHarness(t)
		_, refresh := h.connect(t)

		if a := h.token(t, h.refreshForm(refresh)); a.Status != http.StatusOK {
			t.Fatalf("refresh: %+v", a)
		}

		h.clock.Advance(oauthserver.RefreshGrace)

		// Once the server has read the spent token, the user revokes the
		// grant and approves a new one under the same name.
		var once sync.Once

		h.grants.mu.Lock()
		h.grants.lookedUp = func() {
			once.Do(func() {
				if err := h.grants.RevokeGrant(t.Context(), "chatgpt"); err != nil {
					t.Error(err)
				}

				h.clock.Advance(time.Second)

				if err := h.grants.CreateGrant(t.Context(), store.OAuthGrant{Name: "chatgpt", CreatedAt: h.clock.Now()}); err != nil {
					t.Error(err)
				}
			})
		}
		h.grants.mu.Unlock()

		if a := h.token(t, h.refreshForm(refresh)); a.Error != "invalid_grant" {
			t.Errorf("reuse: %+v, want invalid_grant", a)
		}

		// Only the user's own revoke.
		if diff := cmp.Diff([]string{"chatgpt"}, h.grants.Revoked()); diff != "" {
			t.Errorf("revoked (-want +got):\n%s", diff)
		}

		if !h.grants.hasGrant("chatgpt") {
			t.Error("the re-created grant was revoked")
		}
	})

	t.Run("a lost rotation race is refused, not revoked", func(t *testing.T) {
		h := newHarness(t)
		_, refresh := h.connect(t)
		h.grants.rotateErr = store.ErrSpent

		if a := h.token(t, h.refreshForm(refresh)); a.Error != "invalid_grant" {
			t.Errorf("refresh: %+v, want invalid_grant", a)
		}

		if got := h.grants.Revoked(); len(got) != 0 {
			t.Errorf("revoked %v, want none", got)
		}
	})
}

func TestRefreshRefusals(t *testing.T) {
	tests := []struct {
		name   string
		form   func(f url.Values, access string)
		before func(h *harness)
		status int
		want   string
	}{
		{name: "another allowed client", form: func(f url.Values, _ string) { f.Set("client_id", otherClient) }, want: "invalid_grant"},
		{name: "client removed from clients", before: func(h *harness) {
			h.setServer(func(s *config.Server) { s.OAuth.Clients = []string{otherClient} })
		}, status: http.StatusUnauthorized, want: "invalid_client"},
		{name: "wrong resource", form: func(f url.Values, _ string) { f.Set("resource", "https://w.example") }, want: "invalid_target"},
		{name: "public_url changed", before: func(h *harness) {
			h.setServer(func(s *config.Server) { s.PublicURL = "https://elsewhere.example" })
		}, form: func(f url.Values, _ string) { f.Del("resource") }, want: "invalid_grant"},
		{name: "an access token", form: func(f url.Values, access string) { f.Set("refresh_token", access) }, want: "invalid_grant"},
		{name: "unknown", form: func(f url.Values, _ string) { f.Set("refresh_token", "wf_s_nope") }, want: "invalid_grant"},
		{name: "missing", form: func(f url.Values, _ string) { f.Del("refresh_token") }, want: "invalid_request"},
		{name: "suspended", before: func(h *harness) { h.grants.suspended = true }, want: "invalid_grant"},
		{name: "revoked", before: func(h *harness) { _ = h.grants.RevokeGrant(context.Background(), "chatgpt") }, want: "invalid_grant"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			access, refresh := h.connect(t)

			if tt.before != nil {
				tt.before(h)
			}

			form := h.refreshForm(refresh)
			if tt.form != nil {
				tt.form(form, access)
			}

			a := h.token(t, form)

			status := tt.status
			if status == 0 {
				status = http.StatusBadRequest
			}

			if a.Status != status || a.Error != tt.want || a.AccessToken != "" {
				t.Errorf("answer %+v, want %d %s", a, status, tt.want)
			}

			if got := h.grants.Revoked(); len(got) > 1 || (len(got) == 1 && tt.name != "revoked") {
				t.Errorf("revoked %v", got)
			}
		})
	}
}

func TestTokenRefusesClientAuthentication(t *testing.T) {
	tests := []struct {
		name            string
		header          http.Header
		field           string
		wwwAuthenticate string
	}{
		{name: "Authorization header", header: http.Header{"Authorization": {"Basic Y2xpZW50OnNlY3JldA=="}}, wwwAuthenticate: "Basic"},
		{name: "client_secret", field: "client_secret"},
		{name: "client_assertion", field: "client_assertion"},
		{name: "client_assertion_type", field: "client_assertion_type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			code := h.code(t, "chatgpt")

			form := h.exchangeForm(code)
			if tt.field != "" {
				form.Set(tt.field, "x")
			}

			w := h.tokenRaw(form.Encode(), "application/x-www-form-urlencoded", tt.header)

			a := decodeToken(t, w)
			if a.Status != http.StatusUnauthorized || a.Error != "invalid_client" {
				t.Errorf("answer %+v, want 401 invalid_client", a)
			}

			if got := w.Header().Get("WWW-Authenticate"); got != tt.wwwAuthenticate {
				t.Errorf("WWW-Authenticate %q, want %q", got, tt.wwwAuthenticate)
			}
		})
	}
}

func TestTokenMalformedRequest(t *testing.T) {
	tests := []struct {
		name        string
		body        func(h *harness) string
		contentType string
	}{
		{"oversize body", func(h *harness) string {
			f := h.exchangeForm("c")
			f.Set("pad", strings.Repeat("p", oauthserver.MaxTokenBody))

			return f.Encode()
		}, "application/x-www-form-urlencoded"},
		{"JSON body", func(*harness) string { return `{"grant_type":"authorization_code"}` }, "application/json"},
		{"no content type", func(h *harness) string { return h.exchangeForm("c").Encode() }, ""},
		{"repeated parameter", func(h *harness) string { return h.exchangeForm("c").Encode() + "&code=d" }, "application/x-www-form-urlencoded"},
		{"unparsable", func(h *harness) string { return h.exchangeForm("c").Encode() + "&x=%zz" }, "application/x-www-form-urlencoded"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)

			// Malformed comes before client authentication.
			a := decodeToken(t, h.tokenRaw(tt.body(h), tt.contentType, http.Header{"Authorization": {"Basic eDp5"}}))
			if a.Status != http.StatusBadRequest || a.Error != "invalid_request" {
				t.Errorf("answer %+v, want 400 invalid_request", a)
			}
		})
	}
}

func TestNothingSecretIsLogged(t *testing.T) {
	h := newHarness(t)
	code := h.code(t, "chatgpt")

	a := h.token(t, h.exchangeForm(code))
	if a.Status != http.StatusOK {
		t.Fatalf("exchange: %+v", a)
	}

	b := h.token(t, h.refreshForm(a.RefreshToken))
	h.clock.Advance(time.Minute)
	h.token(t, h.refreshForm(a.RefreshToken)) // reuse, revokes
	h.token(t, h.exchangeForm(code))          // replay
	h.authorize(h.query().Encode(), nil)      // not armed

	logs := h.logs.String()
	if logs == "" {
		t.Fatal("nothing logged")
	}

	for _, secret := range []string{code, state, verifier, challenge, a.AccessToken, a.RefreshToken, b.AccessToken, b.RefreshToken, "redirect_uri", "?"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %q:\n%s", secret, logs)
		}
	}
}

// A code is bound to the grant it was made for, not to the grant's name: a
// grant revoked and re-created under the same name before the code is
// presented (revoke, arm, approve again, all within CodeTTL) gets no tokens
// from it.
func TestExchangeForAGrantReCreatedUnderTheNameIssuesNothing(t *testing.T) {
	h := newHarness(t)
	code := h.code(t, "chatgpt")

	if err := h.grants.RevokeGrant(t.Context(), "chatgpt"); err != nil {
		t.Fatal(err)
	}

	h.clock.Advance(time.Second)

	if _, err := h.srv.Approve(t.Context(), h.claimedID(t, h.query()), "chatgpt", []string{"github"}); err != nil {
		t.Fatalf("Approve of the new grant: %v", err)
	}

	if a := h.token(t, h.exchangeForm(code)); a.Error != "invalid_grant" {
		t.Errorf("exchange of the old grant's code: %+v, want invalid_grant", a)
	}

	if n := h.grants.tokenCount(); n != 0 {
		t.Errorf("%d tokens stored, want none", n)
	}
}

// The revoke a reuse causes outlives its request: a client that goes away
// once it has presented a replayed code or a reused refresh token does not
// call the revoke off.
func TestReuseRevokesEvenWhenTheClientGoesAway(t *testing.T) {
	t.Run("a replayed code", func(t *testing.T) {
		h := newHarness(t)
		code := h.code(t, "chatgpt")

		if a := h.token(t, h.exchangeForm(code)); a.Status != http.StatusOK {
			t.Fatalf("exchange: %+v", a)
		}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		a := decodeToken(t, h.tokenRawCtx(ctx, h.exchangeForm(code).Encode(), "application/x-www-form-urlencoded", nil))
		if a.Error != "invalid_grant" {
			t.Errorf("replay: %+v, want invalid_grant", a)
		}

		if diff := cmp.Diff([]string{"chatgpt"}, h.grants.Revoked()); diff != "" {
			t.Errorf("revoked (-want +got):\n%s", diff)
		}
	})

	t.Run("a refresh token reused after RefreshGrace", func(t *testing.T) {
		h := newHarness(t)
		_, refresh := h.connect(t)

		if a := h.token(t, h.refreshForm(refresh)); a.Status != http.StatusOK {
			t.Fatalf("refresh: %+v", a)
		}

		h.clock.Advance(oauthserver.RefreshGrace)

		ctx, cancel := context.WithCancel(t.Context())
		h.grants.mu.Lock()
		h.grants.lookedUp = cancel
		h.grants.mu.Unlock()

		a := decodeToken(t, h.tokenRawCtx(ctx, h.refreshForm(refresh).Encode(), "application/x-www-form-urlencoded", nil))
		if a.Error != "invalid_grant" {
			t.Errorf("reuse: %+v, want invalid_grant", a)
		}

		if diff := cmp.Diff([]string{"chatgpt"}, h.grants.Revoked()); diff != "" {
			t.Errorf("revoked (-want +got):\n%s", diff)
		}
	})
}
