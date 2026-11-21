package httpd_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/httpd"
)

const scopedToken = "wf_s_nightly-token" //nolint:gosec // G101: a test token, not a credential.

func nightlyClient() clienttoken.Client {
	return clienttoken.Client{Name: "nightly", Connectors: []string{"github", "linear"}}
}

// seen is what clientStub reports about the request it received.
type seen struct {
	Found         bool
	Client        clienttoken.Client
	Authorization []string
}

// clientStub stands in for the gateway: it answers with the Client the SDK
// middleware put in the request's TokenInfo, and the Authorization header
// it sees.
func clientStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := clienttoken.FromTokenInfo(auth.TokenInfoFromContext(r.Context()))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(seen{Found: ok, Client: c, Authorization: r.Header.Values("Authorization")})
	})
}

func newClientsHandler(t *testing.T, token string, reg *clienttoken.Registry, mcpHandler http.Handler) http.Handler {
	t.Helper()
	h, err := httpd.NewHandler(httpd.Config{
		Port:     testPort,
		Token:    token,
		Clients:  reg,
		Version:  testVersion,
		Logger:   zerolog.Nop(),
		Handlers: httpd.Handlers{MCP: mcpHandler, Admin: adminStub()},
	})

	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	return h
}

func nightlyRegistry(now func() time.Time) *clienttoken.Registry {
	return clienttoken.NewRegistry(map[[sha256.Size]byte]clienttoken.Client{
		clienttoken.Digest(scopedToken): nightlyClient(),
	}, now)
}

func bearerRequest(method, path string, auths ...string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), method, "http://127.0.0.1:7777"+path, nil)

	for _, a := range auths {
		req.Header.Add("Authorization", a)
	}

	return req
}

func serveSeen(t *testing.T, h http.Handler, req *http.Request) seen {
	t.Helper()
	code, body := serve(h, req)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", code, body)
	}

	var s seen

	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}

	return s
}

// TestMCPCarriesClient: /mcp hands the gateway the Client its bearer names,
// for the default token and for a scoped one, on every /mcp method.
func TestMCPCarriesClient(t *testing.T) {
	h := newClientsHandler(t, testToken, nightlyRegistry(nil), clientStub())
	tests := []struct {
		name  string
		token string
		want  clienttoken.Client
	}{
		{"default", testToken, clienttoken.Default()},
		{"scoped", scopedToken, nightlyClient()},
	}

	for _, tt := range tests {
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
			t.Run(tt.name+" "+method, func(t *testing.T) {
				got := serveSeen(t, h, bearerRequest(method, "/mcp", "Bearer "+tt.token))
				want := seen{Found: true, Client: tt.want, Authorization: []string{"Bearer " + tt.token}}

				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("handler saw (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// TestMCPWithoutRegistry: a nil Clients keeps today's behaviour, the
// default token only.
func TestMCPWithoutRegistry(t *testing.T) {
	h := newClientsHandler(t, testToken, nil, clientStub())

	if got := serveSeen(t, h, bearerRequest(http.MethodPost, "/mcp", "Bearer "+testToken)); got.Client.Name != clienttoken.DefaultName {
		t.Errorf("client = %+v, want the default", got.Client)
	}

	if code, _ := serve(h, bearerRequest(http.MethodPost, "/mcp", "Bearer "+scopedToken)); code != http.StatusUnauthorized {
		t.Errorf("scoped token with no registry: status = %d, want 401", code)
	}
}

// TestMCPTokenTheSDKWouldNotParse: the SDK middleware splits the header on
// any whitespace, but the guard takes everything after the first space as
// the token. A default token the guard accepts must still reach the
// gateway.
func TestMCPTokenTheSDKWouldNotParse(t *testing.T) {
	const spaced = "wf_two words"
	h := newClientsHandler(t, spaced, nil, clientStub())
	got := serveSeen(t, h, bearerRequest(http.MethodPost, "/mcp", "bearer "+spaced))
	want := seen{Found: true, Client: clienttoken.Default(), Authorization: []string{"bearer " + spaced}}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("handler saw (-want +got):\n%s", diff)
	}
}

func TestBearerRefusals(t *testing.T) {
	h := newClientsHandler(t, testToken, nightlyRegistry(nil), clientStub())
	tests := []struct {
		name  string
		auths []string
	}{
		{"unknown token", []string{"Bearer wf_s_unknown"}},
		{"scoped token twice", []string{"Bearer " + scopedToken, "Bearer " + scopedToken}},
		{"scoped and default", []string{"Bearer " + scopedToken, "Bearer " + testToken}},
		{"scoped without scheme", []string{scopedToken}},
	}

	for _, tt := range tests {
		for _, path := range []string{"/mcp", "/admin/x"} {
			t.Run(tt.name+" "+path, func(t *testing.T) {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, bearerRequest(http.MethodPost, path, tt.auths...))

				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
				}

				if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
					t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
				}
			})
		}
	}
}

// TestAdminRefusesScopedToken: a valid scoped token is told it can't use
// the admin API (403), rather than that it is unknown (401).
func TestAdminRefusesScopedToken(t *testing.T) {
	h := newClientsHandler(t, testToken, nightlyRegistry(nil), clientStub())

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			code, body := serve(h, bearerRequest(method, "/admin/x", "Bearer "+scopedToken))

			if code != http.StatusForbidden || body != "This client token cannot use the admin API.\n" {
				t.Errorf("got %d %q, want 403 with the admin-scope message", code, body)
			}

			code, body = serve(h, bearerRequest(method, "/admin/x", "Bearer "+testToken))

			if want := "admin " + method + " /admin/x"; code != http.StatusOK || body != want {
				t.Errorf("default token: got %d %q, want 200 %q", code, body, want)
			}
		})
	}
}

// TestRevokeAppliesToNextRequest: Remove on the registry the daemon passed
// refuses the very next request, and Add admits one.
func TestRevokeAppliesToNextRequest(t *testing.T) {
	reg := nightlyRegistry(nil)
	h := newClientsHandler(t, testToken, reg, clientStub())
	serveSeen(t, h, bearerRequest(http.MethodPost, "/mcp", "Bearer "+scopedToken))

	reg.Remove("nightly")

	if code, _ := serve(h, bearerRequest(http.MethodPost, "/mcp", "Bearer "+scopedToken)); code != http.StatusUnauthorized {
		t.Errorf("after Remove: status = %d, want 401", code)
	}

	reg.Add(clienttoken.Digest("wf_s_new"), nightlyClient())

	if got := serveSeen(t, h, bearerRequest(http.MethodPost, "/mcp", "Bearer wf_s_new")); got.Client.Name != "nightly" {
		t.Errorf("after Add: client = %+v, want nightly", got.Client)
	}
}

// TestScopedUseIsTouched: a scoped token's use is recorded for the daemon to
// flush as last-used; a refused token's and the default token's are not.
func TestScopedUseIsTouched(t *testing.T) {
	at := time.Date(2026, 11, 8, 22, 0, 4, 0, time.UTC)
	reg := nightlyRegistry(func() time.Time { return at })
	h := newClientsHandler(t, testToken, reg, clientStub())

	serve(h, bearerRequest(http.MethodPost, "/mcp", "Bearer "+testToken))

	if got := reg.Touches(); len(got) != 0 {
		t.Errorf("default token touched %v, want nothing", got)
	}

	serve(h, bearerRequest(http.MethodPost, "/admin/x", "Bearer "+scopedToken))
	serve(h, bearerRequest(http.MethodPost, "/mcp", "Bearer "+scopedToken))

	if diff := cmp.Diff(map[[sha256.Size]byte]time.Time{clienttoken.Digest(scopedToken): at}, reg.Touches()); diff != "" {
		t.Errorf("Touches (-want +got):\n%s", diff)
	}
}
