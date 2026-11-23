package daemon_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
)

// Remote client tokens (ADR 0021) end to end: a real daemon, its gateway,
// and its loopback listener, with requests shaped as a tunnel delivers them.
// httpd's own tests stub the gateway, so only these show that a request the
// host guard admits is served by the gateway too.

// tunnelHost is the public name a tunnel such as ngrok serves the daemon on.
const tunnelHost = "abc.ngrok.app"

// requestWays are the ways a request reaches the daemon: from a local
// client, through a tunnel that keeps the public Host and says it forwarded
// the request (ngrok, Tailscale Funnel, cloudflared by default), and with
// only a foreign Host.
func requestWays() map[string]func(*http.Request) {
	return map[string]func(*http.Request){
		"local": func(*http.Request) {},
		"tunnel": func(r *http.Request) {
			r.Host = tunnelHost
			r.Header.Set("X-Forwarded-Host", tunnelHost)
		},
		"foreign host": func(r *http.Request) { r.Host = tunnelHost },
	}
}

// initialize POSTs an MCP initialize to /mcp with token as the bearer, after
// way shapes the request, and returns the status code and the body.
func (r running) initialize(t *testing.T, token string, way func(*http.Request)) (int, string) {
	t.Helper()
	const body = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"tunnel-test","version":"v0"}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+r.addr+"/mcp", strings.NewReader(body))

	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	way(req)
	// No kept connection: one would hold up the daemon's shutdown.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)

	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(got)
}

// reach returns, for each of tokens (name to value) and each request way,
// whether an initialize reached the gateway ("served"), or else its status
// and body.
func (r running) reach(t *testing.T, tokens map[string]string) map[string]map[string]string {
	t.Helper()
	got := map[string]map[string]string{}

	for name, token := range tokens {
		got[name] = map[string]string{}

		for way, shape := range requestWays() {
			code, body := r.initialize(t, token, shape)

			switch {
			case code == http.StatusOK && strings.Contains(body, `"serverInfo"`):
				got[name][way] = "served"
			default:
				got[name][way] = http.StatusText(code) + ": " + strings.TrimSpace(body)
			}
		}
	}

	return got
}

// createTokens waits for files to be ready, so no connect is still under
// way when the daemon stops, then creates the remote token grok and the
// scoped token nightly, which isn't remote, both for files, and returns
// their values by name, with the default client token as "default".
func createTokens(t *testing.T, h host, r running) map[string]string {
	t.Helper()
	r.waitStatus(t, "files ready", synced("files"))
	def, err := clienttoken.Read(t.Context(), h.env, h.paths)

	if err != nil {
		t.Fatalf("clienttoken.Read: %v", err)
	}

	values := map[string]string{"default": def}

	for name, remote := range map[string]bool{"grok": true, "nightly": false} {
		created, err := r.admin().CreateToken(t.Context(), admin.TokenSpec{Name: name, Connectors: []string{"files"}, Remote: remote})

		if err != nil {
			t.Fatalf("CreateToken(%s): %v", name, err)
		}

		values[name] = created.Token
	}

	return values
}

// remoteReach is what reach returns for createTokens' tokens: grok reaches
// the gateway every way; nightly and the default token only locally, and
// through a tunnel they get the 404 a path that doesn't exist gets.
func remoteReach() map[string]map[string]string {
	const notFound = "Not Found: 404 page not found"

	return map[string]map[string]string{
		"grok":    {"local": "served", "tunnel": "served", "foreign host": "served"},
		"nightly": {"local": "served", "tunnel": notFound, "foreign host": notFound},
		"default": {"local": "served", "tunnel": notFound, "foreign host": notFound},
	}
}

// TestRemoteTokenReachesTheGatewayThroughATunnel: a remote token, from its
// creation, gets an initialize through a tunnel that keeps the public Host
// all the way to the gateway, which answers it, not just past the host
// guard. A scoped token that isn't remote, and the default token, get 404
// that way.
func TestRemoteTokenReachesTheGatewayThroughATunnel(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	h := newHost(t)
	r := serve(t, h, h.options(t), tokenConfig(t))
	tokens := createTokens(t, h, r)

	if diff := cmp.Diff(remoteReach(), r.reach(t, tokens)); diff != "" {
		t.Errorf("initialize by token and way (-want +got):\n%s", diff)
	}
}

// TestRemoteTokensSurviveARestart: a token's remote flag is stored with it,
// so the restarted daemon, which loads its tokens from the store, lets in
// the same requests.
func TestRemoteTokensSurviveARestart(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	h := newHost(t)
	r := serve(t, h, h.options(t), tokenConfig(t))
	tokens := createTokens(t, h, r)
	r.stop()
	r = start(t, h, h.options(t))
	r.waitStatus(t, "files ready", synced("files"))

	if diff := cmp.Diff(remoteReach(), r.reach(t, tokens)); diff != "" {
		t.Errorf("initialize by token and way after a restart (-want +got):\n%s", diff)
	}
}
