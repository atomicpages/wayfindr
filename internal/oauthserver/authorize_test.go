package oauthserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/oauthserver"
	"github.com/atomicpages/wayfindr/internal/store"
)

func TestMetadataDocuments(t *testing.T) {
	h := newHarness(t)

	tests := []struct {
		name    string
		handler http.HandlerFunc
		path    string
		want    map[string]any
	}{
		{
			name:    "resource metadata",
			handler: h.srv.ResourceMetadata,
			path:    oauthserver.ResourceMetadataPath,
			want: map[string]any{
				"resource":                 "https://w.example/mcp",
				"authorization_servers":    []any{"https://w.example"},
				"bearer_methods_supported": []any{"header"},
				"resource_name":            "Wayfindr",
			},
		},
		{
			name:    "resource metadata for /mcp",
			handler: h.srv.ResourceMetadata,
			path:    oauthserver.ResourceMetadataPath + "/mcp",
			want: map[string]any{
				"resource":                 "https://w.example/mcp",
				"authorization_servers":    []any{"https://w.example"},
				"bearer_methods_supported": []any{"header"},
				"resource_name":            "Wayfindr",
			},
		},
		{
			name:    "server metadata",
			handler: h.srv.ServerMetadata,
			path:    oauthserver.ServerMetadataPath,
			want: map[string]any{ //nolint:gosec // G101: endpoint URLs, not credentials
				"issuer":                                         "https://w.example",
				"authorization_endpoint":                         "https://w.example/oauth/authorize",
				"token_endpoint":                                 "https://w.example/oauth/token",
				"response_types_supported":                       []any{"code"},
				"grant_types_supported":                          []any{"authorization_code", "refresh_token"},
				"code_challenge_methods_supported":               []any{"S256"},
				"token_endpoint_auth_methods_supported":          []any{"none"},
				"client_id_metadata_document_supported":          true,
				"authorization_response_iss_parameter_supported": true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tt.handler(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, issuer+tt.path, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}

			headers := map[string]string{
				"Content-Type":  w.Header().Get("Content-Type"),
				"Cache-Control": w.Header().Get("Cache-Control"),
			}
			if diff := cmp.Diff(map[string]string{"Content-Type": "application/json", "Cache-Control": "no-cache"}, headers); diff != "" {
				t.Errorf("headers (-want +got):\n%s", diff)
			}

			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("document (-want +got):\n%s", diff)
			}
		})
	}
}

// TestMetadataUsesTheCanonicalIssuer: a public URL written with an upper-case
// host and the default port still yields the issuer and resource a client
// derives from the address, so its byte-for-byte comparison holds.
func TestMetadataUsesTheCanonicalIssuer(t *testing.T) {
	h := newHarness(t)
	h.setServer(func(s *config.Server) { s.PublicURL = "HTTPS://W.Example:443/" })

	w := httptest.NewRecorder()
	h.srv.ServerMetadata(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, issuer+oauthserver.ServerMetadataPath, nil))

	var doc struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}

	if doc.Issuer != "https://w.example" || doc.AuthorizationEndpoint != "https://w.example/oauth/authorize" {
		t.Errorf("issuer %q, authorization_endpoint %q, want the lower-case host with no port", doc.Issuer, doc.AuthorizationEndpoint)
	}

	w = httptest.NewRecorder()
	h.srv.ResourceMetadata(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, issuer+oauthserver.ResourceMetadataPath, nil))

	var prm struct {
		Resource string `json:"resource"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &prm); err != nil {
		t.Fatal(err)
	}

	if prm.Resource != "https://w.example/mcp" {
		t.Errorf("resource %q, want https://w.example/mcp", prm.Resource)
	}
}

func TestEveryHandlerIs404WhileOff(t *testing.T) {
	h := newHarness(t)
	h.setServer(func(s *config.Server) { s.OAuth.Enabled = false })
	h.srv.Arm()

	tests := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		target  string
	}{
		{"resource metadata", h.srv.ResourceMetadata, http.MethodGet, oauthserver.ResourceMetadataPath},
		{"server metadata", h.srv.ServerMetadata, http.MethodGet, oauthserver.ServerMetadataPath},
		{"authorize", h.srv.Authorize, http.MethodGet, oauthserver.AuthorizePath + "?" + h.query().Encode()},
		{"token", h.srv.Token, http.MethodPost, oauthserver.TokenPath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tt.handler(w, httptest.NewRequestWithContext(context.Background(), tt.method, issuer+tt.target, nil))

			if w.Code != http.StatusNotFound || w.Body.String() != "404 page not found\n" {
				t.Errorf("got %d %q, want the 404", w.Code, w.Body.String())
			}
		})
	}

	if n := h.fetches(); n != 0 {
		t.Errorf("%d fetches while off", n)
	}
}

func TestAuthorizeClientNotAllowedFetchesNothing(t *testing.T) {
	tests := []struct {
		name  string
		query func(h *harness) string
	}{
		{"unlisted", func(h *harness) string {
			q := h.query()
			q.Set("client_id", "https://evil.example/client.json")

			return q.Encode()
		}},
		{"missing", func(h *harness) string {
			q := h.query()
			q.Del("client_id")

			return q.Encode()
		}},
		{"repeated", func(h *harness) string {
			q := h.query()
			q.Add("client_id", h.clientID)

			return q.Encode()
		}},
		{"unparsable query", func(h *harness) string {
			return h.query().Encode() + "&x=%zz"
		}},
		{"listed with a different case", func(h *harness) string {
			q := h.query()
			q.Set("client_id", strings.ToUpper(h.clientID))

			return q.Encode()
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.Arm()

			// Also cross-site and no-cors: client_not_allowed comes first.
			w := h.authorize(tt.query(h), http.Header{"Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"no-cors"}})

			if got := pageCase(w); got != "client_not_allowed" {
				t.Errorf("case %q, want client_not_allowed", got)
			}

			if n := h.fetches(); n != 0 {
				t.Errorf("%d fetches, want 0", n)
			}

			// No pending authorization, and the arm is still there.
			if id := h.authorizeID(t, h.query()); id == "" {
				t.Error("the arm was used up")
			}
		})
	}
}

func TestAuthorizeErrorPage(t *testing.T) {
	tests := []struct {
		name   string
		query  func(q url.Values)
		header http.Header
		raw    string // appended to the query
		serve  func(h *harness) http.HandlerFunc
		want   string
		// fetched is whether the CIMD document is fetched before the
		// refusal.
		fetched bool
	}{
		{name: "cross-site no-cors", header: http.Header{"Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"no-cors"}}, want: "not_navigation"},
		{name: "cross-site cors", header: http.Header{"Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"cors"}}, want: "not_navigation"},
		{name: "same-site no-cors", header: http.Header{"Sec-Fetch-Site": {"same-site"}, "Sec-Fetch-Mode": {"no-cors"}}, want: "not_navigation"},
		{name: "cross-site iframe", header: http.Header{"Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"iframe"}}, want: "not_navigation"},
		{name: "same-origin iframe", header: http.Header{"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"iframe"}}, want: "not_navigation"},
		{name: "navigation without a destination", header: http.Header{"Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"navigate"}}, want: "not_navigation"},
		{name: "Sec-Fetch-Dest alone", header: http.Header{"Sec-Fetch-Dest": {"image"}}, want: "not_navigation"},
		{name: "prefetch", header: http.Header{"Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}, "Sec-Purpose": {"prefetch"}}, want: "not_navigation"},
		{name: "prerender", header: http.Header{"Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}, "Sec-Purpose": {"prefetch;prerender"}}, want: "not_navigation"},
		{name: "legacy Purpose prefetch", header: http.Header{"Purpose": {"prefetch"}}, want: "not_navigation"},
		{name: "repeated state", raw: "&state=again", want: "duplicate_parameter"},
		{name: "repeated scope", raw: "&scope=again", want: "duplicate_parameter"},
		{name: "response_type token", query: func(q url.Values) { q.Set("response_type", "token") }, want: "unsupported_response_type"},
		{name: "no response_type", query: func(q url.Values) { q.Del("response_type") }, want: "unsupported_response_type"},
		{name: "plain method", query: func(q url.Values) { q.Set("code_challenge_method", "plain") }, want: "invalid_pkce"},
		{name: "no method", query: func(q url.Values) { q.Del("code_challenge_method") }, want: "invalid_pkce"},
		{name: "short challenge", query: func(q url.Values) { q.Set("code_challenge", challenge[:42]) }, want: "invalid_pkce"},
		{name: "challenge not base64url", query: func(q url.Values) { q.Set("code_challenge", challenge[:42]+"+") }, want: "invalid_pkce"},
		{name: "no resource", query: func(q url.Values) { q.Del("resource") }, want: "invalid_resource"},
		{name: "other resource", query: func(q url.Values) { q.Set("resource", "https://w.example/mcp/") }, want: "invalid_resource"},
		{name: "no state", query: func(q url.Values) { q.Del("state") }, want: "missing_state"},
		{name: "empty state", query: func(q url.Values) { q.Set("state", "") }, want: "missing_state"},
		{name: "state over 2 KiB", query: func(q url.Values) { q.Set("state", strings.Repeat("s", 2049)) }, want: "missing_state"},
		{name: "no redirect_uri", query: func(q url.Values) { q.Del("redirect_uri") }, want: "invalid_redirect_uri"},
		{name: "http redirect", query: func(q url.Values) { q.Set("redirect_uri", "http://chatgpt.example/cb") }, want: "invalid_redirect_uri"},
		{name: "relative redirect", query: func(q url.Values) { q.Set("redirect_uri", "/cb") }, want: "invalid_redirect_uri"},
		{name: "redirect with fragment", query: func(q url.Values) { q.Set("redirect_uri", redirectURI+"#x") }, want: "invalid_redirect_uri"},
		{name: "javascript redirect", query: func(q url.Values) { q.Set("redirect_uri", "javascript:alert(1)") }, want: "invalid_redirect_uri"},
		{name: "redirect not listed", query: func(q url.Values) { q.Set("redirect_uri", "https://chatgpt.example/other") }, want: "redirect_not_registered", fetched: true},
		{name: "loopback redirect not listed", query: func(q url.Values) { q.Set("redirect_uri", "http://127.0.0.1:9/cb") }, want: "redirect_not_registered", fetched: true},
		{
			name: "document unavailable",
			serve: func(*harness) http.HandlerFunc {
				return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
			},
			want:    "client_document",
			fetched: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if tt.serve != nil {
				h.serve(tt.serve(h))
			}

			h.srv.Arm()

			q := h.query()
			if tt.query != nil {
				tt.query(q)
			}

			w := h.authorize(q.Encode()+tt.raw, tt.header)

			if got := pageCase(w); got != tt.want {
				t.Fatalf("status %d, case %q, want %q", w.Code, got, tt.want)
			}

			wantHeaders := map[string]string{
				"Content-Type":            "text/html; charset=utf-8",
				"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'; base-uri 'none'",
				"Cache-Control":           "no-store",
				"X-Content-Type-Options":  "nosniff",
				"Referrer-Policy":         "no-referrer",
				"Location":                "",
			}

			got := map[string]string{}
			for k := range wantHeaders {
				got[k] = w.Header().Get(k)
			}

			if diff := cmp.Diff(wantHeaders, got); diff != "" {
				t.Errorf("headers (-want +got):\n%s", diff)
			}

			if strings.Contains(w.Body.String(), state) || strings.Contains(w.Body.String(), "chatgpt.example") {
				t.Error("the page echoes the request")
			}

			if fetched := h.fetches() > 0; fetched != tt.fetched {
				t.Errorf("fetched %v, want %v", fetched, tt.fetched)
			}

			// A refusal leaves the arm for the next request. (A failed
			// document stays cached, so that case can't show it.)
			if id := h.authorizeID(t, h.query()); id == "" && tt.want != "client_document" {
				t.Error("the refusal used up the arm")
			}
		})
	}
}

func TestAuthorizeNotArmed(t *testing.T) {
	t.Run("never armed", func(t *testing.T) {
		h := newHarness(t)

		// Also a repeated parameter: not_armed comes first.
		w := h.authorize(h.query().Encode()+"&state=x", nil)
		if got := pageCase(w); got != "not_armed" {
			t.Errorf("case %q, want not_armed", got)
		}

		if !strings.Contains(w.Body.String(), "Wayfindr isn&#39;t expecting a connection. Open Wayfindr&#39;s console, choose Connect, then try again in the app.") {
			t.Errorf("page lacks the not_armed sentence: %s", w.Body.String())
		}

		if n := h.fetches(); n != 0 {
			t.Errorf("%d fetches, want 0", n)
		}
	})

	t.Run("arm expired", func(t *testing.T) {
		h := newHarness(t)
		h.srv.Arm()
		h.clock.Advance(oauthserver.PendingTTL)

		if got := pageCase(h.authorize(h.query().Encode(), nil)); got != "not_armed" {
			t.Errorf("case %q, want not_armed", got)
		}

		if n := h.fetches(); n != 0 {
			t.Errorf("%d fetches, want 0", n)
		}
	})

	t.Run("a claim ends the arm", func(t *testing.T) {
		h := newHarness(t)

		if want, got := start.Add(oauthserver.PendingTTL), h.srv.Arm(); !got.Equal(want) {
			t.Errorf("Arm = %v, want %v", got, want)
		}

		id := h.authorizeID(t, h.query())
		if id == "" {
			t.Fatal("first request refused")
		}

		if h.authorizeID(t, h.query()) == "" {
			t.Fatal("a second request on the same arm was refused")
		}

		if _, ok := h.srv.Claim(id); !ok {
			t.Fatal("Claim: false")
		}

		if got := pageCase(h.authorize(h.query().Encode(), nil)); got != "not_armed" {
			t.Errorf("case %q after the claim, want not_armed", got)
		}
	})

	t.Run("arming again resets the expiry", func(t *testing.T) {
		h := newHarness(t)
		h.srv.Arm()
		h.clock.Advance(9 * time.Minute)
		h.srv.Arm()
		h.clock.Advance(9 * time.Minute)

		if h.authorizeID(t, h.query()) == "" {
			t.Error("refused 9 minutes after the second arm")
		}
	})
}

func TestAuthorizeSecFetch(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
	}{
		{"no Sec-Fetch headers", nil},
		{"cross-site top-level navigation", http.Header{"Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}, "Sec-Fetch-User": {"?1"}}},
		{"typed into the address bar", http.Header{"Sec-Fetch-Site": {"none"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}}},
		{"a Purpose that is not a prefetch", http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}, "Purpose": {"other"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.Arm()

			w := h.authorize(h.query().Encode(), tt.header)
			if w.Code != http.StatusFound {
				t.Errorf("status %d, case %q, want 302", w.Code, pageCase(w))
			}
		})
	}
}

func TestAuthorizeKeepsAPendingAuthorization(t *testing.T) {
	h := newHarness(t)
	h.srv.Arm()
	h.clock.Advance(time.Second)

	w := h.authorize(h.query().Encode(), http.Header{"X-Forwarded-For": {"203.0.113.7"}})
	if w.Code != http.StatusFound {
		t.Fatalf("status %d, case %q", w.Code, pageCase(w))
	}

	wantHeaders := map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer"}

	gotHeaders := map[string]string{"Cache-Control": w.Header().Get("Cache-Control"), "Referrer-Policy": w.Header().Get("Referrer-Policy")}
	if diff := cmp.Diff(wantHeaders, gotHeaders); diff != "" {
		t.Errorf("headers (-want +got):\n%s", diff)
	}

	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	id := loc.Query().Get("req")
	if len(id) < 26 || w.Header().Get("Location") != approveURL(id) {
		t.Fatalf("Location %q is not the approve page", w.Header().Get("Location"))
	}

	// Pending reads only a claimed request, and Approve and Deny answer
	// only one.
	if _, ok := h.srv.Pending(id); ok {
		t.Error("Pending of an unclaimed request: true")
	}

	if _, err := h.srv.Approve(t.Context(), id, "chatgpt", []string{"github"}); !errors.Is(err, oauthserver.ErrNoRequest) {
		t.Errorf("Approve of an unclaimed request: %v, want ErrNoRequest", err)
	}

	if _, err := h.srv.Deny(id); !errors.Is(err, oauthserver.ErrNoRequest) {
		t.Errorf("Deny of an unclaimed request: %v, want ErrNoRequest", err)
	}

	want := oauthserver.Request{
		ID:           id,
		ClientID:     h.clientID,
		ClientName:   "ChatGPT",
		ClientHost:   h.clientHost,
		RedirectHost: "chatgpt.example",
		StartedAt:    start.Add(time.Second),
		ReportedIP:   "203.0.113.7",
		ExpiresAt:    start.Add(time.Second + oauthserver.PendingTTL),
		Arrived:      1,
	}

	got, ok := h.srv.Claim(id)
	if !ok {
		t.Fatal("Claim: false")
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Claim (-want +got):\n%s", diff)
	}

	// A reload claims it again, and Pending now reads it.
	if got, ok := h.srv.Claim(id); !ok || got != want {
		t.Errorf("second Claim = %+v, %v; want the request again", got, ok)
	}

	if got, ok := h.srv.Pending(id); !ok || got != want {
		t.Errorf("Pending = %+v, %v; want the claimed request", got, ok)
	}
}

// While armed, every valid request is kept pending; the console's claim of
// one consumes the arm and drops the others, so a stranger's request made
// in the same minutes can never be answered once the user has claimed
// theirs. Nor can the claimed one be approved when it was not the only one
// (TestApproveContested).
func TestClaim(t *testing.T) {
	t.Run("drops the other pending authorizations", func(t *testing.T) {
		h := newHarness(t)
		h.srv.Arm()

		stranger := h.authorizeID(t, h.query())
		user := h.authorizeID(t, h.query())
		later := h.authorizeID(t, h.query())

		if stranger == "" || user == "" || later == "" {
			t.Fatal("a request on the arm was refused")
		}

		if _, ok := h.srv.Claim(user); !ok {
			t.Fatal("Claim of the user's request: false")
		}

		// Armed again, the stranger's requests are not back.
		h.srv.Arm()

		for _, id := range []string{stranger, later} {
			if _, ok := h.srv.Claim(id); ok {
				t.Error("Claim of a dropped request: true")
			}

			if _, ok := h.srv.Pending(id); ok {
				t.Error("Pending of a dropped request: true")
			}

			if _, err := h.srv.Approve(t.Context(), id, "stranger", []string{"github"}); !errors.Is(err, oauthserver.ErrNoRequest) {
				t.Errorf("Approve of a dropped request: %v, want ErrNoRequest", err)
			}

			if _, err := h.srv.Deny(id); !errors.Is(err, oauthserver.ErrNoRequest) {
				t.Errorf("Deny of a dropped request: %v, want ErrNoRequest", err)
			}
		}

		// Three arrived on the user's arm, so the claimed one is not
		// approved either; Deny still answers it.
		if _, err := h.srv.Approve(t.Context(), user, "chatgpt", []string{"github"}); !errors.Is(err, oauthserver.ErrContested) {
			t.Errorf("Approve of the claimed request: %v, want ErrContested", err)
		}

		if _, err := h.srv.Deny(user); err != nil {
			t.Errorf("Deny of the claimed request: %v", err)
		}
	})

	t.Run("a stranger's request made after the claim is refused", func(t *testing.T) {
		h := newHarness(t)
		user := h.claimedID(t, h.query())

		if got := pageCase(h.authorize(h.query().Encode(), nil)); got != "not_armed" {
			t.Errorf("case %q, want not_armed", got)
		}

		if _, ok := h.srv.Pending(user); !ok {
			t.Error("the claimed request is gone")
		}
	})

	t.Run("a reload has no other effect", func(t *testing.T) {
		h := newHarness(t)
		first := h.claimedID(t, h.query())

		// Armed again, a new request is pending beside the claimed one.
		second := h.pendingID(t, h.query())

		if _, ok := h.srv.Claim(first); !ok {
			t.Fatal("Claim of the claimed request: false")
		}

		// The reload consumed nothing: still armed, the new one still
		// pending, so it can be claimed (Claim needs the arm).
		if _, ok := h.srv.Claim(second); !ok {
			t.Fatal("Claim of the new request: false (the reload ended the arm)")
		}

		// The new claim replaces the earlier one.
		if _, err := h.srv.Approve(t.Context(), first, "chatgpt", []string{"github"}); !errors.Is(err, oauthserver.ErrNoRequest) {
			t.Errorf("Approve of the replaced claim: %v, want ErrNoRequest", err)
		}

		if _, err := h.srv.Approve(t.Context(), second, "chatgpt", []string{"github"}); err != nil {
			t.Errorf("Approve of the new claim: %v", err)
		}
	})

	t.Run("unknown id", func(t *testing.T) {
		h := newHarness(t)
		h.pendingID(t, h.query())

		if _, ok := h.srv.Claim("not-the-id"); ok {
			t.Error("Claim of an unknown id: true")
		}

		if h.authorizeID(t, h.query()) == "" {
			t.Error("a failed Claim ended the arm")
		}
	})

	t.Run("after the arm expired", func(t *testing.T) {
		h := newHarness(t)
		h.srv.Arm()
		h.clock.Advance(oauthserver.PendingTTL - time.Minute)

		id := h.authorizeID(t, h.query())
		if id == "" {
			t.Fatal("refused while armed")
		}

		// The request has 9 minutes left, but the arm has ended.
		h.clock.Advance(time.Minute)

		if _, ok := h.srv.Claim(id); ok {
			t.Error("Claim after the arm ended: true")
		}

		// Nor does a new arm bring it back.
		h.srv.Arm()

		if _, ok := h.srv.Claim(id); ok {
			t.Error("Claim on a new arm of the old arm's request: true")
		}
	})

	t.Run("past MaxPending the oldest goes", func(t *testing.T) {
		h := newHarness(t)
		h.srv.Arm()

		ids := make([]string, oauthserver.MaxPending+1)
		for i := range ids {
			if ids[i] = h.authorizeID(t, h.query()); ids[i] == "" {
				t.Fatalf("request %d refused", i)
			}
		}

		if _, ok := h.srv.Claim(ids[0]); ok {
			t.Error("Claim of the evicted request: true")
		}

		if _, ok := h.srv.Claim(ids[1]); !ok {
			t.Error("Claim of the oldest request kept: false")
		}
	})

	t.Run("racing claims of two requests", func(t *testing.T) {
		for range 20 {
			h := newHarness(t)
			h.srv.Arm()

			ids := []string{h.authorizeID(t, h.query()), h.authorizeID(t, h.query())}
			oks := make([]bool, 2)
			begin := make(chan struct{})

			var wg sync.WaitGroup

			for i, id := range ids {
				wg.Go(func() {
					<-begin

					_, oks[i] = h.srv.Claim(id)
				})
			}

			close(begin)
			wg.Wait()

			if oks[0] == oks[1] {
				t.Fatalf("claims %v, want exactly one", oks)
			}
		}
	})
}

func TestAuthorizeReportedIP(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   string
	}{
		{"none", nil, ""},
		// A browser can send CF-Connecting-IP and Cf-Ray itself, so
		// neither counts, together or apart.
		{"CF-Connecting-IP with Cf-Ray is ignored", http.Header{"Cf-Connecting-Ip": {"198.51.100.4"}, "Cf-Ray": {"8f1e2d3c4b5a6978-AMS"}, "X-Forwarded-For": {"192.0.2.1"}}, "192.0.2.1"},
		{"CF-Connecting-IP without Cf-Ray is ignored", http.Header{"Cf-Connecting-Ip": {"198.51.100.4"}, "X-Forwarded-For": {"192.0.2.1"}}, "192.0.2.1"},
		{"CF-Connecting-IP and Cf-Ray without X-Forwarded-For", http.Header{"Cf-Connecting-Ip": {"198.51.100.4"}, "Cf-Ray": {"8f1e2d3c4b5a6978-AMS"}}, ""},
		{"last X-Forwarded-For", http.Header{"X-Forwarded-For": {"10.0.0.1 , 2001:db8::1 "}}, "2001:db8::1"},
		{"entries the browser prepended", http.Header{"X-Forwarded-For": {"198.51.100.66, 203.0.113.9"}}, "203.0.113.9"},
		{"last of several X-Forwarded-For headers", http.Header{"X-Forwarded-For": {"198.51.100.66", "203.0.113.9"}}, "203.0.113.9"},
		{"malformed", http.Header{"X-Forwarded-For": {"<script>"}}, ""},
		{"malformed last entry", http.Header{"X-Forwarded-For": {"192.0.2.1, nope"}}, ""},
		{"with a zone", http.Header{"X-Forwarded-For": {"fe80::1%eth0"}}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.Arm()

			w := h.authorize(h.query().Encode(), tt.header)
			if w.Code != http.StatusFound {
				t.Fatalf("status %d", w.Code)
			}

			loc, _ := url.Parse(w.Header().Get("Location"))

			req, ok := h.srv.Claim(loc.Query().Get("req"))
			if !ok {
				t.Fatal("no pending authorization")
			}

			if req.ReportedIP != tt.want {
				t.Errorf("ReportedIP = %q, want %q", req.ReportedIP, tt.want)
			}
		})
	}
}

func TestApprove(t *testing.T) {
	h := newHarness(t)
	id := h.claimedID(t, h.query())

	if _, err := h.srv.Approve(t.Context(), "not-the-id", "chatgpt", []string{"github"}); !errors.Is(err, oauthserver.ErrNoRequest) {
		t.Errorf("Approve of an unknown id: %v, want ErrNoRequest", err)
	}

	h.clock.Advance(2 * time.Second)

	redirect, err := h.srv.Approve(t.Context(), id, "chatgpt", []string{"github", "linear"})
	if err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}

	if got := u.Scheme + "://" + u.Host + u.Path; got != redirectURI {
		t.Errorf("redirect goes to %q", got)
	}

	q := u.Query()
	if q.Get("code") == "" || q.Get("state") != state || q.Get("iss") != issuer || len(q) != 3 {
		t.Errorf("redirect query %v, want code, state, iss", q)
	}

	g, err := h.grants.Grant(t.Context(), "chatgpt")
	if err != nil {
		t.Fatal(err)
	}

	want := store.OAuthGrant{
		Name:       "chatgpt",
		ClientID:   h.clientID,
		Resource:   resource,
		Connectors: []string{"github", "linear"},
		CreatedAt:  start.Add(2 * time.Second),
	}
	if diff := cmp.Diff(want, g); diff != "" {
		t.Errorf("grant (-want +got):\n%s", diff)
	}

	if _, ok := h.srv.Pending(id); ok {
		t.Error("still pending after Approve")
	}

	if _, err := h.srv.Approve(t.Context(), id, "chatgpt-2", []string{"github"}); !errors.Is(err, oauthserver.ErrNoRequest) {
		t.Errorf("second Approve: %v, want ErrNoRequest", err)
	}

	if _, err := h.srv.Deny(id); !errors.Is(err, oauthserver.ErrNoRequest) {
		t.Errorf("Deny after Approve: %v, want ErrNoRequest", err)
	}
}

func TestApproveKeepsTheRedirectQuery(t *testing.T) {
	h := newHarness(t)
	q := h.query()
	q.Set("redirect_uri", "https://chatgpt.example/cb?x=1")

	redirect, err := h.srv.Approve(t.Context(), h.claimedID(t, q), "chatgpt", []string{"github"})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(redirect, "https://chatgpt.example/cb?x=1&") {
		t.Errorf("redirect %q does not keep the redirect_uri's query", redirect)
	}

	u, _ := url.Parse(redirect)
	if u.Query().Get("x") != "1" || u.Query().Get("iss") != issuer {
		t.Errorf("redirect query %v", u.Query())
	}
}

func TestApproveTakenNameKeepsTheRequest(t *testing.T) {
	h := newHarness(t)

	if err := h.grants.CreateGrant(t.Context(), store.OAuthGrant{Name: "chatgpt"}); err != nil {
		t.Fatal(err)
	}

	id := h.claimedID(t, h.query())

	if _, err := h.srv.Approve(t.Context(), id, "chatgpt", []string{"github"}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("Approve: %v, want ErrExists", err)
	}

	if _, ok := h.srv.Pending(id); !ok {
		t.Fatal("the request is no longer pending")
	}

	if _, err := h.srv.Approve(t.Context(), id, "chatgpt-2", []string{"github"}); err != nil {
		t.Errorf("Approve with another name: %v", err)
	}
}

func TestDeny(t *testing.T) {
	h := newHarness(t)
	id := h.claimedID(t, h.query())

	redirect, err := h.srv.Deny(id)
	if err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}

	want := url.Values{"error": {"access_denied"}, "state": {state}, "iss": {issuer}}
	if diff := cmp.Diff(want, u.Query()); diff != "" {
		t.Errorf("redirect query (-want +got):\n%s", diff)
	}

	if _, ok := h.srv.Pending(id); ok {
		t.Error("still pending after Deny")
	}

	if _, err := h.srv.Approve(t.Context(), id, "chatgpt", []string{"github"}); !errors.Is(err, oauthserver.ErrNoRequest) {
		t.Errorf("Approve after Deny: %v, want ErrNoRequest", err)
	}
}

func TestPendingNoLongerCurrent(t *testing.T) {
	tests := []struct {
		name   string
		change func(h *harness)
	}{
		{"expired", func(h *harness) { h.clock.Advance(oauthserver.PendingTTL) }},
		{"turned off", func(h *harness) { h.setServer(func(s *config.Server) { s.OAuth.Enabled = false }) }},
		{"public_url changed", func(h *harness) {
			h.setServer(func(s *config.Server) { s.PublicURL = "https://elsewhere.example" })
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			id := h.claimedID(t, h.query())
			tt.change(h)

			if _, ok := h.srv.Pending(id); ok {
				t.Error("Pending says true")
			}

			if _, err := h.srv.Approve(t.Context(), id, "chatgpt", []string{"github"}); !errors.Is(err, oauthserver.ErrNoRequest) {
				t.Errorf("Approve: %v, want ErrNoRequest", err)
			}

			if _, err := h.srv.Deny(id); !errors.Is(err, oauthserver.ErrNoRequest) {
				t.Errorf("Deny: %v, want ErrNoRequest", err)
			}
		})
	}
}

// Only one authorization request is expected per arm, the user's own. When
// more arrive, the one claimed may be a stranger's, opened in a tab the user
// didn't open, so Approve refuses it whichever is claimed; Deny still
// answers it, and the next arm counts from zero.
func TestApproveContested(t *testing.T) {
	for _, pick := range []int{0, 1} {
		t.Run(fmt.Sprintf("claim request %d of 2", pick+1), func(t *testing.T) {
			h := newHarness(t)
			h.srv.Arm()

			ids := []string{h.authorizeID(t, h.query()), h.authorizeID(t, h.query())}
			if ids[0] == "" || ids[1] == "" {
				t.Fatal("a request on the arm was refused")
			}

			req, ok := h.srv.Claim(ids[pick])
			if !ok {
				t.Fatal("Claim: false")
			}

			if req.Arrived != 2 {
				t.Errorf("Arrived = %d, want 2", req.Arrived)
			}

			if _, err := h.srv.Approve(t.Context(), ids[pick], "chatgpt", []string{"github"}); !errors.Is(err, oauthserver.ErrContested) {
				t.Fatalf("Approve: %v, want ErrContested", err)
			}

			if h.grants.hasGrant("chatgpt") {
				t.Error("a refused Approve created the grant")
			}

			// Nothing changed: still claimed, a reload reads the same count.
			if got, ok := h.srv.Pending(ids[pick]); !ok || got.Arrived != 2 {
				t.Errorf("Pending = %+v, %v; want the claimed request with Arrived 2", got, ok)
			}

			redirect, err := h.srv.Deny(ids[pick])
			if err != nil {
				t.Fatalf("Deny: %v", err)
			}

			if u, _ := url.Parse(redirect); u.Query().Get("error") != "access_denied" {
				t.Errorf("Deny redirect %q lacks access_denied", redirect)
			}

			// The retry: a new arm with one request is approved.
			id := h.claimedID(t, h.query())

			if got, _ := h.srv.Pending(id); got.Arrived != 1 {
				t.Errorf("Arrived on the next arm = %d, want 1", got.Arrived)
			}

			if _, err := h.srv.Approve(t.Context(), id, "chatgpt", []string{"github"}); err != nil {
				t.Errorf("Approve on the next arm: %v", err)
			}
		})
	}

	t.Run("arming again keeps the count", func(t *testing.T) {
		h := newHarness(t)
		h.pendingID(t, h.query())
		id := h.pendingID(t, h.query())

		if _, ok := h.srv.Claim(id); !ok {
			t.Fatal("Claim: false")
		}

		if _, err := h.srv.Approve(t.Context(), id, "chatgpt", []string{"github"}); !errors.Is(err, oauthserver.ErrContested) {
			t.Errorf("Approve: %v, want ErrContested", err)
		}
	})

	t.Run("evicted requests count", func(t *testing.T) {
		h := newHarness(t)
		h.srv.Arm()

		var last string
		for range oauthserver.MaxPending + 1 {
			last = h.authorizeID(t, h.query())
		}

		req, ok := h.srv.Claim(last)
		if !ok {
			t.Fatal("Claim: false")
		}

		if req.Arrived != oauthserver.MaxPending+1 {
			t.Errorf("Arrived = %d, want %d", req.Arrived, oauthserver.MaxPending+1)
		}
	})
}

// Disarm forgets everything armed or answered under the settings it was
// made with: the daemon calls it when a reload turns the server off,
// changes public_url, or changes the clients.
func TestDisarm(t *testing.T) {
	t.Run("the claimed request and the arm", func(t *testing.T) {
		h := newHarness(t)
		id := h.claimedID(t, h.query())
		h.srv.Arm()

		// Turned off and on again within PendingTTL.
		h.setServer(func(s *config.Server) { s.OAuth.Enabled = false })
		h.srv.Disarm()
		h.setServer(func(s *config.Server) { s.OAuth.Enabled = true })
		h.srv.Disarm()

		if _, ok := h.srv.Claim(id); ok {
			t.Error("Claim after Disarm: true")
		}

		if _, err := h.srv.Approve(t.Context(), id, "chatgpt", []string{"github"}); !errors.Is(err, oauthserver.ErrNoRequest) {
			t.Errorf("Approve after Disarm: %v, want ErrNoRequest", err)
		}

		if _, err := h.srv.Deny(id); !errors.Is(err, oauthserver.ErrNoRequest) {
			t.Errorf("Deny after Disarm: %v, want ErrNoRequest", err)
		}

		if got := pageCase(h.authorize(h.query().Encode(), nil)); got != "not_armed" {
			t.Errorf("authorize after Disarm: case %q, want not_armed", got)
		}
	})

	t.Run("pending requests", func(t *testing.T) {
		h := newHarness(t)
		id := h.pendingID(t, h.query())
		h.srv.Disarm()
		h.srv.Arm()

		if _, ok := h.srv.Claim(id); ok {
			t.Error("Claim of a request pending before Disarm: true")
		}
	})

	t.Run("codes not yet presented", func(t *testing.T) {
		h := newHarness(t)
		c := h.code(t, "chatgpt")
		h.srv.Disarm()

		if a := h.token(t, h.exchangeForm(c)); a.Status != http.StatusBadRequest || a.Error != "invalid_grant" {
			t.Errorf("exchange after Disarm: %d %q, want 400 invalid_grant", a.Status, a.Error)
		}
	})
}

// A claimed request whose client was taken out of server.oauth.clients is
// not approved, even before the daemon's Disarm.
func TestApproveClientRemoved(t *testing.T) {
	h := newHarness(t)
	id := h.claimedID(t, h.query())
	h.setServer(func(s *config.Server) { s.OAuth.Clients = []string{otherClient} })

	if _, err := h.srv.Approve(t.Context(), id, "removed-client", []string{"github"}); !errors.Is(err, oauthserver.ErrNoRequest) {
		t.Errorf("Approve: %v, want ErrNoRequest", err)
	}

	if h.grants.hasGrant("removed-client") {
		t.Error("the grant was created")
	}
}

// A HEAD, such as a link unfurler's, is not an authorization request: it
// gets 405, fetches nothing, and leaves the arm for the user's own GET.
func TestAuthorizeHeadLeavesTheArm(t *testing.T) {
	h := newHarness(t)
	h.srv.Arm()

	r := httptest.NewRequestWithContext(context.Background(), http.MethodHead, issuer+oauthserver.AuthorizePath+"?"+h.query().Encode(), nil)
	w := httptest.NewRecorder()
	h.srv.Authorize(w, r)

	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
		t.Errorf("HEAD: %d, Allow %q; want 405, Allow GET", w.Code, w.Header().Get("Allow"))
	}

	if n := h.fetches(); n != 0 {
		t.Errorf("%d fetches for a HEAD, want 0", n)
	}

	if w := h.authorize(h.query().Encode(), nil); w.Code != http.StatusFound {
		t.Errorf("GET after the HEAD: %d, case %q; want 302 (the arm is still there)", w.Code, pageCase(w))
	}
}

// A pending authorization is answered once: of two racing answers, Approve
// and Approve or Approve and Deny, exactly one wins and the other gets
// ErrNoRequest.
func TestRacingAnswersToOneRequest(t *testing.T) {
	answers := map[string]func(h *harness, id string) error{
		"approve": func(h *harness, id string) error {
			_, err := h.srv.Approve(context.Background(), id, "chatgpt", []string{"github"})

			return err
		},
		"approve as another name": func(h *harness, id string) error {
			_, err := h.srv.Approve(context.Background(), id, "chatgpt-2", []string{"github"})

			return err
		},
		"deny": func(h *harness, id string) error {
			_, err := h.srv.Deny(id)

			return err
		},
	}

	for _, pair := range [][2]string{{"approve", "approve as another name"}, {"approve", "deny"}} {
		t.Run(pair[0]+" and "+pair[1], func(t *testing.T) {
			for range 20 {
				h := newHarness(t)
				id := h.claimedID(t, h.query())
				errs := make([]error, 2)
				begin := make(chan struct{})

				var wg sync.WaitGroup

				for i, name := range pair {
					wg.Go(func() {
						<-begin

						errs[i] = answers[name](h, id)
					})
				}

				close(begin)
				wg.Wait()

				won, lost := 0, 0

				for _, err := range errs {
					switch {
					case err == nil:
						won++
					case errors.Is(err, oauthserver.ErrNoRequest):
						lost++
					default:
						t.Fatalf("answer: %v, want success or ErrNoRequest", err)
					}
				}

				if won != 1 || lost != 1 {
					t.Fatalf("%d won and %d got ErrNoRequest, want one each", won, lost)
				}

				if _, ok := h.srv.Pending(id); ok {
					t.Fatal("still pending after it was answered")
				}
			}
		})
	}
}
