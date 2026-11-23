package oauthserver_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/oauthserver"
)

// clientName authorizes with an arm and returns the pending
// authorization's client name, or fails with the page's case.
func (h *harness) clientName(t *testing.T) string {
	t.Helper()

	req, ok := h.srv.Pending(h.claimedID(t, h.query()))
	if !ok {
		t.Fatal("no pending authorization")
	}

	return req.ClientName
}

func TestCIMDCache(t *testing.T) {
	t.Run("a valid document is reused for DocumentTTL", func(t *testing.T) {
		h := newHarness(t)

		var accept atomic.Value

		h.serve(func(w http.ResponseWriter, r *http.Request) {
			accept.Store(r.Header.Get("Accept"))
			_ = json.NewEncoder(w).Encode(h.document())
		})

		h.clientName(t)
		h.clientName(t)

		if n := h.fetches(); n != 1 {
			t.Errorf("%d fetches, want 1", n)
		}

		if got := accept.Load(); got != "application/json" {
			t.Errorf("Accept %q", got)
		}

		h.clock.Advance(oauthserver.DocumentTTL)
		h.clientName(t)

		if n := h.fetches(); n != 2 {
			t.Errorf("%d fetches after DocumentTTL, want 2", n)
		}
	})

	t.Run("a failure is remembered for FailureTTL", func(t *testing.T) {
		h := newHarness(t)
		h.serve(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
		h.srv.Arm()

		for range 2 {
			if got := pageCase(h.authorize(h.query().Encode(), nil)); got != "client_document" {
				t.Fatalf("case %q, want client_document", got)
			}
		}

		if n := h.fetches(); n != 1 {
			t.Errorf("%d fetches, want 1", n)
		}

		if !strings.Contains(h.logs.String(), `"reason":"status"`) || !strings.Contains(h.logs.String(), `"status":503`) {
			t.Errorf("the failure is not logged with its reason: %s", h.logs.String())
		}

		h.serve(h.serveDocument(h.document()))
		h.clock.Advance(oauthserver.FailureTTL)

		if h.authorizeID(t, h.query()) == "" {
			t.Error("still refused after FailureTTL")
		}

		if n := h.fetches(); n != 2 {
			t.Errorf("%d fetches after FailureTTL, want 2", n)
		}
	})
}

func TestCIMDRefusals(t *testing.T) {
	tests := []struct {
		name string
		// doc changes the valid document; serve, when set, replaces the
		// handler.
		doc   func(h *harness, d map[string]any)
		serve func(h *harness) http.HandlerFunc
	}{
		{name: "client_id not the URL fetched", doc: func(h *harness, d map[string]any) { d["client_id"] = h.clientID + "/" }},
		{name: "no client_id", doc: func(_ *harness, d map[string]any) { delete(d, "client_id") }},
		{name: "client_id not a string", doc: func(_ *harness, d map[string]any) { d["client_id"] = 7 }},
		{name: "no redirect_uris", doc: func(_ *harness, d map[string]any) { delete(d, "redirect_uris") }},
		{name: "empty redirect_uris", doc: func(_ *harness, d map[string]any) { d["redirect_uris"] = []string{} }},
		{name: "redirect_uris not strings", doc: func(_ *harness, d map[string]any) { d["redirect_uris"] = []int{1} }},
		{name: "auth method not none", doc: func(_ *harness, d map[string]any) { d["token_endpoint_auth_method"] = "private_key_jwt" }},
		{name: "auth methods without none", doc: func(_ *harness, d map[string]any) {
			delete(d, "token_endpoint_auth_method")
			d["token_endpoint_auth_methods_supported"] = []string{"private_key_jwt"}
		}},
		{name: "client_name not a string", doc: func(_ *harness, d map[string]any) { d["client_name"] = 1 }},
		{name: "oversize", serve: func(h *harness) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(padded(t, h.document(), oauthserver.MaxDocument+1))
			}
		}},
		{name: "a redirect", serve: func(*harness) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/client.json" {
					http.Redirect(w, r, "/real.json", http.StatusFound)

					return
				}

				_, _ = w.Write([]byte("{}"))
			}
		}},
		{name: "not 200", serve: func(h *harness) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(h.document())
			}
		}},
		{name: "two objects", serve: func(h *harness) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(h.document())
				_ = json.NewEncoder(w).Encode(h.document())
			}
		}},
		{name: "an array", serve: func(h *harness) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode([]any{h.document()})
			}
		}},
		{name: "not JSON", serve: func(*harness) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)

			switch {
			case tt.serve != nil:
				h.serve(tt.serve(h))
			case tt.doc != nil:
				d := h.document()
				tt.doc(h, d)
				h.serve(h.serveDocument(d))
			}

			h.srv.Arm()

			if got := pageCase(h.authorize(h.query().Encode(), nil)); got != "client_document" {
				t.Errorf("case %q, want client_document", got)
			}

			if n := h.fetches(); n != 1 {
				t.Errorf("%d fetches, want 1 (no redirect followed)", n)
			}
		})
	}
}

// padded is doc as JSON, padded with spaces to exactly n bytes.
func padded(t *testing.T, doc any, n int) []byte {
	t.Helper()

	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	return append(b, []byte(strings.Repeat(" ", n-len(b)))...)
}

func TestCIMDAccepts(t *testing.T) {
	tests := []struct {
		name  string
		doc   func(h *harness, d map[string]any)
		serve func(h *harness) http.HandlerFunc
		want  func(h *harness) string
	}{
		{name: "exactly MaxDocument bytes", serve: func(h *harness) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(padded(t, h.document(), oauthserver.MaxDocument))
			}
		}, want: func(*harness) string { return "ChatGPT" }},
		{name: "none in the plural field only", doc: func(_ *harness, d map[string]any) {
			d["token_endpoint_auth_method"] = "private_key_jwt"
			d["token_endpoint_auth_methods_supported"] = []string{"private_key_jwt", "none"}
		}, want: func(*harness) string { return "ChatGPT" }},
		{name: "no auth method fields", doc: func(_ *harness, d map[string]any) {
			delete(d, "token_endpoint_auth_method")
		}, want: func(*harness) string { return "ChatGPT" }},
		{name: "no client_name: the host", doc: func(_ *harness, d map[string]any) {
			delete(d, "client_name")
		}, want: func(h *harness) string { return h.clientHost }},
		{name: "client_name with controls", doc: func(_ *harness, d map[string]any) {
			d["client_name"] = "Chat\x1b[31mGPT\u202e"
		}, want: func(*harness) string { return "Chat\ufffd[31mGPT\ufffd" }},
		{name: "long client_name clipped at a rune", doc: func(_ *harness, d map[string]any) {
			d["client_name"] = strings.Repeat("a", 99) + "é"
		}, want: func(*harness) string { return strings.Repeat("a", 99) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)

			switch {
			case tt.serve != nil:
				h.serve(tt.serve(h))
			case tt.doc != nil:
				d := h.document()
				tt.doc(h, d)
				h.serve(h.serveDocument(d))
			}

			if got, want := h.clientName(t), tt.want(h); got != want {
				t.Errorf("ClientName %q, want %q", got, want)
			}
		})
	}
}

func TestCIMDRefusesHTTPClientID(t *testing.T) {
	h := newHarness(t)
	insecure := strings.Replace(h.clientID, "https://", "http://", 1)
	h.setServer(func(s *config.Server) { s.OAuth.Clients = append(s.OAuth.Clients, insecure) })
	h.srv.Arm()

	q := h.query()
	q.Set("client_id", insecure)

	if got := pageCase(h.authorize(q.Encode(), nil)); got != "client_document" {
		t.Errorf("case %q, want client_document", got)
	}

	if n := h.fetches(); n != 0 {
		t.Errorf("%d fetches, want 0", n)
	}
}

func TestCIMDPublicOnlyRefusesLoopback(t *testing.T) {
	// No Options.Client: New falls back to httpclient.PublicOnly, which
	// refuses the test server's loopback address at connect time.
	h := newHarness(t, func(o *oauthserver.Options) { o.Client = nil })
	h.srv.Arm()

	if got := pageCase(h.authorize(h.query().Encode(), nil)); got != "client_document" {
		t.Errorf("case %q, want client_document", got)
	}

	if !strings.Contains(h.logs.String(), `"reason":"not_public"`) {
		t.Errorf("the refusal is not logged as not_public: %s", h.logs.String())
	}

	if strings.Contains(h.logs.String(), url.QueryEscape(state)) {
		t.Error("the log holds the query")
	}
}
