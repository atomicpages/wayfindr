package daemon_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/httpd"
	"github.com/atomicpages/wayfindr/internal/oauthserver"
)

// Wayfindr's authorization server's routes through the running daemon
// (ADR 0022, design §7.11): with [server.oauth] on, a request through a
// tunnel reaches the metadata documents, authorize, and the token endpoint,
// and a tokenless /mcp gets the challenge naming the metadata; turned off,
// each gets the 404 again.

// TestOAuthPathsAgree: httpd names the authorization server's paths itself,
// since it can't import oauthserver (design §2); they must be the same.
func TestOAuthPathsAgree(t *testing.T) {
	got := []string{httpd.ResourceMetadataPath, httpd.ServerMetadataPath, httpd.AuthorizePath, httpd.TokenPath}
	want := []string{oauthserver.ResourceMetadataPath, oauthserver.ServerMetadataPath, oauthserver.AuthorizePath, oauthserver.TokenPath}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("httpd's paths differ from oauthserver's (-want +got):\n%s", diff)
	}
}

// answer is what a client sees of the daemon's answer.
type answer struct {
	Code      int
	Challenge string
	Body      string
}

// throughTunnel sends method path to r as a tunnel delivers it (the public
// URL's Host, flowIssuer's, and X-Forwarded-Host), with body as a form and
// auth as the Authorization header when they are not empty.
func (r running) throughTunnel(t *testing.T, method, path, body, auth string) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+r.addr+path, strings.NewReader(body))

	if err != nil {
		t.Fatal(err)
	}

	host := strings.TrimPrefix(flowIssuer, "https://")
	req.Host = host
	req.Header.Set("X-Forwarded-Host", host)

	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	if auth != "" {
		req.Header.Set("Authorization", auth)
	}

	// No kept connection: one would hold up the daemon's shutdown.
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)

	if err != nil {
		t.Fatal(err)
	}

	return answer{Code: resp.StatusCode, Challenge: resp.Header.Get("WWW-Authenticate"), Body: string(got)}
}

func TestOAuthRoutesThroughATunnelWhileOnAndNotOff(t *testing.T) {
	h := newHost(t)
	initHost(t, h, grantsConfig(t))
	access, _ := seedGrant(t, h)
	r := start(t, h, h.options(t))
	metadata := flowIssuer + "/.well-known/oauth-protected-resource/mcp"

	for _, path := range []string{httpd.ResourceMetadataPath, httpd.ResourceMetadataPath + "/mcp"} {
		got := r.throughTunnel(t, http.MethodGet, path, "", "")

		if got.Code != http.StatusOK {
			t.Fatalf("GET %s through the tunnel = %+v, want 200", path, got)
		}

		var doc map[string]any

		if err := json.Unmarshal([]byte(got.Body), &doc); err != nil {
			t.Fatalf("GET %s: %v in %q", path, err, got.Body)
		}

		want := map[string]any{
			"resource":                 flowIssuer + "/mcp",
			"authorization_servers":    []any{flowIssuer},
			"bearer_methods_supported": []any{"header"},
			"resource_name":            "Wayfindr",
		}

		if diff := cmp.Diff(want, doc); diff != "" {
			t.Errorf("GET %s (-want +got):\n%s", path, diff)
		}
	}

	got := r.throughTunnel(t, http.MethodGet, httpd.ServerMetadataPath, "", "")
	var doc map[string]any

	if err := json.Unmarshal([]byte(got.Body), &doc); got.Code != http.StatusOK || err != nil {
		t.Fatalf("GET %s through the tunnel = %+v (%v), want 200 JSON", httpd.ServerMetadataPath, got, err)
	}

	want := map[string]any{
		"issuer":                                         flowIssuer,
		"authorization_endpoint":                         flowIssuer + "/oauth/authorize",
		"token_endpoint":                                 flowIssuer + "/oauth/token",
		"response_types_supported":                       []any{"code"},
		"grant_types_supported":                          []any{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []any{"S256"},
		"token_endpoint_auth_methods_supported":          []any{"none"},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	}

	if diff := cmp.Diff(want, doc); diff != "" {
		t.Errorf("GET %s (-want +got):\n%s", httpd.ServerMetadataPath, diff)
	}

	// Authorize and the token endpoint are the authorization server's own:
	// not armed, and no client_id.
	if got := r.throughTunnel(t, http.MethodGet, httpd.AuthorizePath+"?client_id="+flowClient, "", ""); got.Code != http.StatusBadRequest || !strings.Contains(got.Body, `data-case="not_armed"`) {
		t.Errorf("GET authorize through the tunnel = %+v, want the not_armed error page", got)
	}

	if got := r.throughTunnel(t, http.MethodPost, httpd.TokenPath, "grant_type=refresh_token", ""); got.Code != http.StatusBadRequest || !strings.Contains(got.Body, `"error":"invalid_request"`) {
		t.Errorf("POST token through the tunnel = %+v, want invalid_request", got)
	}

	challenges := map[string]answer{
		"no token":    {http.StatusUnauthorized, `Bearer resource_metadata="` + metadata + `"`, "unauthorized\n"},
		"wrong token": {http.StatusUnauthorized, `Bearer resource_metadata="` + metadata + `", error="invalid_token"`, "unauthorized\n"},
	}

	for name, want := range challenges {
		auth := ""

		if name == "wrong token" {
			auth = "Bearer wf_s_wrong"
		}

		if diff := cmp.Diff(want, r.throughTunnel(t, http.MethodPost, "/mcp", `{}`, auth)); diff != "" {
			t.Errorf("POST /mcp through the tunnel, %s (-want +got):\n%s", name, diff)
		}
	}

	// A DNS-rebinding page's request names its own Host and no forwarding
	// header: it learns nothing, public_url least of all.
	for _, path := range []string{httpd.ResourceMetadataPath + "/mcp", httpd.ServerMetadataPath, "/mcp"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+r.addr+path, nil)

		if err != nil {
			t.Fatal(err)
		}

		req.Host = "rebind.evil.example:" + strings.Split(r.addr, ":")[1]

		if got := roundTrip(t, req); got.code != http.StatusNotFound || got.header.Get("WWW-Authenticate") != "" || strings.Contains(got.body, "wf.example.com") {
			t.Errorf("GET %s with a rebinding Host = %d %q, want the plain 404", path, got.code, got.body)
		}
	}

	// The grant's access token reaches the gateway through the tunnel.
	if code, body := r.initialize(t, access, requestWays()["tunnel"]); code != http.StatusOK || !strings.Contains(body, `"serverInfo"`) {
		t.Errorf("initialize with the grant's access token through the tunnel = %d %q, want served", code, body)
	}

	if _, err := r.admin().SetOAuthEnabled(t.Context(), false); err != nil {
		t.Fatalf("SetOAuthEnabled(false): %v", err)
	}

	notFound := answer{Code: http.StatusNotFound, Body: "404 page not found\n"}

	for _, req := range []struct{ method, path, body, auth string }{
		{http.MethodGet, httpd.ResourceMetadataPath, "", ""},
		{http.MethodGet, httpd.ResourceMetadataPath + "/mcp", "", ""},
		{http.MethodGet, httpd.ServerMetadataPath, "", ""},
		{http.MethodGet, httpd.AuthorizePath + "?client_id=" + flowClient, "", ""},
		{http.MethodPost, httpd.TokenPath, "grant_type=refresh_token", ""},
		{http.MethodPost, "/mcp", `{}`, ""},
		{http.MethodPost, "/mcp", `{}`, "Bearer wf_s_wrong"},
		{http.MethodPost, "/mcp", `{}`, "Bearer " + access},
	} {
		if diff := cmp.Diff(notFound, r.throughTunnel(t, req.method, req.path, req.body, req.auth)); diff != "" {
			t.Errorf("with OAuth off, %s %s through the tunnel (-want +got):\n%s", req.method, req.path, diff)
		}
	}
}

// TestApprovePageIsNotServedThroughATunnel: while on, the console's approve
// page, the admin API, and the console are still loopback only.
func TestApprovePageIsNotServedThroughATunnel(t *testing.T) {
	h := newHost(t)
	initHost(t, h, grantsConfig(t))
	r := start(t, h, h.options(t))
	notFound := answer{Code: http.StatusNotFound, Body: "404 page not found\n"}

	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/ui/oauth/approve?req=r1"},
		{http.MethodPost, "/ui/oauth/approve"},
		{http.MethodGet, "/ui/"},
		{http.MethodPost, "/admin/oauth/arm"},
		{http.MethodGet, "/admin/oauth/requests/r1"},
		{http.MethodGet, "/healthz"},
		{http.MethodPost, httpd.AuthorizePath},
		{http.MethodGet, httpd.TokenPath},
	} {
		if diff := cmp.Diff(notFound, r.throughTunnel(t, req.method, req.path, "", "")); diff != "" {
			t.Errorf("%s %s through the tunnel (-want +got):\n%s", req.method, req.path, diff)
		}
	}
}
