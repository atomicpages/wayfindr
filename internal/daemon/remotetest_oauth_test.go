package daemon_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// The connection test with OAuth on (ADR 0022, design §7.11): a test with no
// token fetches the discovery documents through the tunnel and checks that
// they name the public URL, and a test with a token reads Wayfindr's own 401
// challenge as an invalid token. A TLS server stands in for the tunnel and
// for the Wayfindr behind it, answering what httpd answers with OAuth on.

const (
	prmPath = "/.well-known/oauth-protected-resource/mcp"
	asPath  = "/.well-known/oauth-authorization-server"
)

// oauthTunnel is a Wayfindr with OAuth on, seen through a tunnel. Each field
// replaces one of its answers; a nil one is the correct answer.
type oauthTunnel struct {
	// url is the tunnel's address, the issuer a correct Wayfindr names.
	url func() string
	// bare answers POST /mcp with no Authorization header.
	bare http.HandlerFunc
	// bearer answers POST /mcp with one.
	bearer http.HandlerFunc
	// prm and as answer the two discovery documents.
	prm, as http.HandlerFunc

	mu   sync.Mutex
	seen []string
}

// requests is what the tunnel saw, in order: method, path, and whether the
// request carried an Authorization header.
func (o *oauthTunnel) requests() []string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return append([]string(nil), o.seen...)
}

// challenge is Wayfindr's 401 for a remote request to /mcp with OAuth on.
func (o *oauthTunnel) challenge(extra string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+o.url()+prmPath+`"`+extra)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func jsonDoc(doc string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, doc)
	}
}

func (o *oauthTunnel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := "no-auth"

	if r.Header.Get("Authorization") != "" {
		auth = "auth"
	}

	o.mu.Lock()
	o.seen = append(o.seen, r.Method+" "+r.URL.Path+" "+auth)
	o.mu.Unlock()

	issuer := o.url()

	pick := func(custom http.HandlerFunc, correct http.HandlerFunc) {
		if custom != nil {
			custom(w, r)

			return
		}

		correct(w, r)
	}

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/mcp" && auth == "auth":
		pick(o.bearer, o.challenge(`, error="invalid_token"`))
	case r.Method == http.MethodPost && r.URL.Path == "/mcp":
		pick(o.bare, o.challenge(""))
	case r.Method == http.MethodGet && r.URL.Path == prmPath:
		pick(o.prm, jsonDoc(`{"resource":"`+issuer+`/mcp","authorization_servers":["`+issuer+`"],"bearer_methods_supported":["header"],"resource_name":"Wayfindr"}`))
	case r.Method == http.MethodGet && r.URL.Path == asPath:
		pick(o.as, jsonDoc(`{"issuer":"`+issuer+`","authorization_endpoint":"`+issuer+`/oauth/authorize","token_endpoint":"`+issuer+`/oauth/token",`+
			`"response_types_supported":["code"],"grant_types_supported":["authorization_code","refresh_token"],`+
			`"code_challenge_methods_supported":["S256"],"token_endpoint_auth_methods_supported":["none"]}`))
	default:
		http.NotFound(w, r)
	}
}

// startOAuthTunnel serves o over TLS, with o.url reading the server's own
// address.
func startOAuthTunnel(t *testing.T, o *oauthTunnel) *httptest.Server {
	t.Helper()

	srv := httptest.NewUnstartedServer(o)
	o.url = func() string { return srv.URL }

	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv
}

// remoteTestOAuth sets the public URL to url, turns OAuth on, and runs the
// OAuth test.
func (r running) remoteTestOAuth(t *testing.T, url string) admin.RemoteTestResult {
	t.Helper()

	if _, err := r.admin().SetPublicURL(t.Context(), url); err != nil {
		t.Fatalf("SetPublicURL(%q): %v", url, err)
	}

	if _, err := r.admin().SetOAuthEnabled(t.Context(), true); err != nil {
		t.Fatalf("SetOAuthEnabled(true): %v", err)
	}

	res, err := r.admin().TestRemoteOAuth(t.Context())

	if err != nil {
		t.Fatalf("TestRemoteOAuth: %v", err)
	}

	return res
}

// TestRemoteOAuthConnectionOutcomes: what the discovery documents and the
// challenge can look like through a tunnel, each as its own outcome.
func TestRemoteOAuthConnectionOutcomes(t *testing.T) {
	anchor := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(anchor.Close)

	h := newHost(t)
	opts := h.options(t)
	opts.RemoteTestClient = trustingClient(anchor)
	opts.RemoteTestTimeout = 400 * time.Millisecond
	r := serve(t, h, opts, tokenConfig(t))

	// A target a challenge or a document points at: nothing may reach it.
	var (
		mu   sync.Mutex
		hits int
	)

	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	t.Cleanup(target.Close)

	ownChallengeAt := func(url string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+url+`"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}

	headerChallenge := func(value string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", value)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}

	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", code) }
	}

	htmlPage := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, "<html><body>Sign in to continue</body></html>")
	}

	redirect := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/login?next=secret-query", http.StatusFound)
	}

	const (
		okAS   = `"response_types_supported":["code"],"grant_types_supported":["authorization_code"],"token_endpoint_auth_methods_supported":["none"]`
		okPRMT = `"bearer_methods_supported":["header"]`
	)

	// docs builds a document from the tunnel's address, as a handler.
	docs := func(f func(issuer string) string) func(o *oauthTunnel) http.HandlerFunc {
		return func(o *oauthTunnel) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, f(o.url()))
			}
		}
	}

	tests := []struct {
		name string
		// shape sets the tunnel's answers; nil is a correct Wayfindr.
		shape func(o *oauthTunnel)
		want  outcome
		// requests is what the tunnel must have seen, in order, when set.
		requests []string
	}{
		{
			name:  "a correct tunnel: the challenge names the metadata, and both documents name the public URL",
			shape: func(*oauthTunnel) {},
			want:  outcome{Outcome: admin.RemoteOK, OK: true},
			requests: []string{
				"POST /mcp no-auth",
				"GET " + prmPath + " no-auth",
				"GET " + asPath + " no-auth",
			},
		},
		{
			name: "a challenge with the parameters in another order, a lower-case scheme, and an escape, is still ours",
			shape: func(o *oauthTunnel) {
				o.bare = func(w http.ResponseWriter, r *http.Request) { o.reorderedChallenge(w, r) }
			},
			want: outcome{Outcome: admin.RemoteOK, OK: true},
		},
		{
			name:  "exactly Bearer: the proxy makes the internet look local",
			shape: func(o *oauthTunnel) { o.bare = headerChallenge("Bearer") },
			want:  outcome{Outcome: admin.RemoteProxyTreatsRemoteAsLocal, Status: http.StatusUnauthorized},
			requests: []string{
				"POST /mcp no-auth",
			},
		},
		{
			name:  "a challenge naming another address's metadata: metadata_mismatch, and that address is never fetched",
			shape: func(o *oauthTunnel) { o.bare = ownChallengeAt(target.URL + prmPath) },
			want:  outcome{Outcome: admin.RemoteMetadataMismatch, Status: http.StatusUnauthorized},
			requests: []string{
				"POST /mcp no-auth",
			},
		},
		{
			name: "a challenge naming this host's metadata at another path is not ours: metadata_mismatch",
			shape: func(o *oauthTunnel) {
				o.bare = func(w http.ResponseWriter, r *http.Request) { ownChallengeAt(o.url()+"/other")(w, r) }
			},
			want: outcome{Outcome: admin.RemoteMetadataMismatch, Status: http.StatusUnauthorized},
		},
		{
			name:  "a proxy's Bearer realm with no metadata is not Wayfindr's: unexpected",
			shape: func(o *oauthTunnel) { o.bare = headerChallenge(`Bearer realm="corp-proxy", error="invalid_token"`) },
			want:  outcome{Outcome: admin.RemoteUnexpected, Status: http.StatusUnauthorized},
		},
		{
			name:  "a proxy's Basic challenge: unexpected",
			shape: func(o *oauthTunnel) { o.bare = headerChallenge(`Basic realm="corp-proxy"`) },
			want:  outcome{Outcome: admin.RemoteUnexpected, Status: http.StatusUnauthorized},
		},
		{
			name:  "a 401 with no challenge: unexpected",
			shape: func(o *oauthTunnel) { o.bare = status(http.StatusUnauthorized) },
			want:  outcome{Outcome: admin.RemoteUnexpected, Status: http.StatusUnauthorized},
		},
		{
			name:  "Wayfindr's own 404 to the bare initialize (OAuth off, or an older daemon): metadata_missing",
			shape: func(o *oauthTunnel) { o.bare = notFound },
			want:  outcome{Outcome: admin.RemoteMetadataMissing, Status: http.StatusNotFound},
		},
		{
			name:  "ngrok's offline 404 is the tunnel, not missing metadata: unreachable",
			shape: func(o *oauthTunnel) { o.bare = ngrokOffline },
			want:  outcome{Outcome: admin.RemoteUnreachable, Status: http.StatusNotFound},
		},
		{
			name:  "a tunnel's HTML page for the bare initialize: interstitial",
			shape: func(o *oauthTunnel) { o.bare = htmlPage },
			want:  outcome{Outcome: admin.RemoteInterstitial, Status: http.StatusOK},
		},
		{
			name:  "a redirect for the bare initialize is reported and not followed",
			shape: func(o *oauthTunnel) { o.bare = redirect },
			want:  outcome{Outcome: admin.RemoteRedirect, Status: http.StatusFound},
		},
		{
			name:  "a 200 for the bare initialize: unexpected",
			shape: func(o *oauthTunnel) { o.bare = func(http.ResponseWriter, *http.Request) {} },
			want:  outcome{Outcome: admin.RemoteUnexpected, Status: http.StatusOK},
		},
		{
			name:  "a 5xx for the bare initialize: unexpected",
			shape: func(o *oauthTunnel) { o.bare = status(http.StatusBadGateway) },
			want:  outcome{Outcome: admin.RemoteUnexpected, Status: http.StatusBadGateway},
		},
		{
			name:  "the protected resource metadata is a 404: metadata_missing",
			shape: func(o *oauthTunnel) { o.prm = notFound },
			want:  outcome{Outcome: admin.RemoteMetadataMissing, Status: http.StatusNotFound},
			requests: []string{
				"POST /mcp no-auth",
				"GET " + prmPath + " no-auth",
			},
		},
		{
			name: "the protected resource metadata is not JSON: metadata_missing",
			shape: func(o *oauthTunnel) {
				o.prm = func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "hello") }
			},
			want: outcome{Outcome: admin.RemoteMetadataMissing, Status: http.StatusOK},
		},
		{
			name:  "the protected resource metadata is a 5xx: unexpected",
			shape: func(o *oauthTunnel) { o.prm = status(http.StatusInternalServerError) },
			want:  outcome{Outcome: admin.RemoteUnexpected, Status: http.StatusInternalServerError},
		},
		{
			name:  "the protected resource metadata is a web page: interstitial",
			shape: func(o *oauthTunnel) { o.prm = htmlPage },
			want:  outcome{Outcome: admin.RemoteInterstitial, Status: http.StatusOK},
		},
		{
			name:  "the protected resource metadata redirects: reported, never followed",
			shape: func(o *oauthTunnel) { o.prm = redirect },
			want:  outcome{Outcome: admin.RemoteRedirect, Status: http.StatusFound},
		},
		{
			name: "the protected resource names another resource: metadata_mismatch",
			shape: func(o *oauthTunnel) {
				o.prm = docs(func(i string) string {
					return `{"resource":"` + target.URL + `/mcp","authorization_servers":["` + i + `"],` + okPRMT + `}`
				})(o)
			},
			want: outcome{Outcome: admin.RemoteMetadataMismatch, Status: http.StatusOK},
		},
		{
			name: "the protected resource names the host with no /mcp: metadata_mismatch",
			shape: func(o *oauthTunnel) {
				o.prm = docs(func(i string) string {
					return `{"resource":"` + i + `","authorization_servers":["` + i + `"],` + okPRMT + `}`
				})(o)
			},
			want: outcome{Outcome: admin.RemoteMetadataMismatch, Status: http.StatusOK},
		},
		{
			name: "the protected resource names another authorization server: metadata_mismatch, and it is never fetched",
			shape: func(o *oauthTunnel) {
				o.prm = docs(func(i string) string {
					return `{"resource":"` + i + `/mcp","authorization_servers":["` + target.URL + `"],` + okPRMT + `}`
				})(o)
			},
			want: outcome{Outcome: admin.RemoteMetadataMismatch, Status: http.StatusOK},
		},
		{
			name: "the protected resource names two authorization servers: metadata_mismatch",
			shape: func(o *oauthTunnel) {
				o.prm = docs(func(i string) string {
					return `{"resource":"` + i + `/mcp","authorization_servers":["` + i + `","` + target.URL + `"],` + okPRMT + `}`
				})(o)
			},
			want: outcome{Outcome: admin.RemoteMetadataMismatch, Status: http.StatusOK},
		},
		{
			name:  "the authorization server metadata is a 404: metadata_missing",
			shape: func(o *oauthTunnel) { o.as = notFound },
			want:  outcome{Outcome: admin.RemoteMetadataMissing, Status: http.StatusNotFound},
		},
		{
			name: "the authorization server names another issuer: metadata_mismatch",
			shape: func(o *oauthTunnel) {
				o.as = docs(func(i string) string {
					return `{"issuer":"` + target.URL + `","authorization_endpoint":"` + i + `/oauth/authorize","token_endpoint":"` + i + `/oauth/token",` +
						`"code_challenge_methods_supported":["S256"],` + okAS + `}`
				})(o)
			},
			want: outcome{Outcome: admin.RemoteMetadataMismatch, Status: http.StatusOK},
		},
		{
			name: "the authorization endpoint is on another host: metadata_mismatch",
			shape: func(o *oauthTunnel) {
				o.as = docs(func(i string) string {
					return `{"issuer":"` + i + `","authorization_endpoint":"` + target.URL + `/oauth/authorize","token_endpoint":"` + i + `/oauth/token",` +
						`"code_challenge_methods_supported":["S256"],` + okAS + `}`
				})(o)
			},
			want: outcome{Outcome: admin.RemoteMetadataMismatch, Status: http.StatusOK},
		},
		{
			name: "the token endpoint is on another host: metadata_mismatch",
			shape: func(o *oauthTunnel) {
				o.as = docs(func(i string) string {
					return `{"issuer":"` + i + `","authorization_endpoint":"` + i + `/oauth/authorize","token_endpoint":"` + target.URL + `/oauth/token",` +
						`"code_challenge_methods_supported":["S256"],` + okAS + `}`
				})(o)
			},
			want: outcome{Outcome: admin.RemoteMetadataMismatch, Status: http.StatusOK},
		},
		{
			name: "S256 not listed: metadata_mismatch",
			shape: func(o *oauthTunnel) {
				o.as = docs(func(i string) string {
					return `{"issuer":"` + i + `","authorization_endpoint":"` + i + `/oauth/authorize","token_endpoint":"` + i + `/oauth/token",` +
						`"code_challenge_methods_supported":["plain"],` + okAS + `}`
				})(o)
			},
			want: outcome{Outcome: admin.RemoteMetadataMismatch, Status: http.StatusOK},
		},
		{
			name: "a server that never answers times out",
			shape: func(o *oauthTunnel) {
				o.bare = func(_ http.ResponseWriter, r *http.Request) {
					// The body first: the server notices a closed connection
					// only once it has read it.
					_, _ = io.Copy(io.Discard, r.Body)
					<-r.Context().Done()
				}
			},
			want: outcome{Outcome: admin.RemoteTimeout},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &oauthTunnel{}
			tt.shape(o)

			tunnel := startOAuthTunnel(t, o)
			res := r.remoteTestOAuth(t, tunnel.URL)

			if diff := cmp.Diff(tt.want, outcomeOf(res)); diff != "" {
				t.Errorf("outcome (-want +got):\n%s\nresult: %+v", diff, res)
			}

			if res.Message == "" {
				t.Error("no message")
			}

			if !res.OK && res.Action == "" {
				t.Error("a failure with no action")
			}

			if res.Outcome == admin.RemoteOK && res.URL != tunnel.URL+"/mcp" {
				t.Errorf("URL = %q, want %q", res.URL, tunnel.URL+"/mcp")
			}

			if strings.Contains(res.Message+res.Action, "secret-query") {
				t.Errorf("the result repeats a redirect's query: %+v", res)
			}

			if tt.requests != nil {
				if diff := cmp.Diff(tt.requests, o.requests()); diff != "" {
					t.Errorf("requests the tunnel saw (-want +got):\n%s", diff)
				}
			}
		})
	}

	t.Run("nothing named by an answer is ever fetched", func(t *testing.T) {
		mu.Lock()
		defer mu.Unlock()

		if hits != 0 {
			t.Errorf("an address named by an answer was reached %d times", hits)
		}
	})

	t.Run("OAuth off is oauth_off and sends nothing", func(t *testing.T) {
		o := &oauthTunnel{}
		tunnel := startOAuthTunnel(t, o)

		if _, err := r.admin().SetPublicURL(t.Context(), tunnel.URL); err != nil {
			t.Fatal(err)
		}

		if _, err := r.admin().SetOAuthEnabled(t.Context(), false); err != nil {
			t.Fatal(err)
		}

		res, err := r.admin().TestRemoteOAuth(t.Context())

		if err != nil || res.Outcome != admin.RemoteOAuthOff || res.OK || res.Message == "" || res.Action == "" {
			t.Errorf("TestRemoteOAuth = %+v, %v; want oauth_off", res, err)
		}

		if got := o.requests(); len(got) != 0 {
			t.Errorf("the tunnel saw %v, want nothing", got)
		}
	})

	t.Run("nothing answers at a closed port", func(t *testing.T) {
		tunnel := httptest.NewTLSServer(http.NotFoundHandler())
		public := tunnel.URL
		tunnel.Close()

		res := r.remoteTestOAuth(t, public)

		if got := outcomeOf(res); got != (outcome{Outcome: admin.RemoteUnreachable}) {
			t.Errorf("outcome = %+v, want unreachable", got)
		}
	})

	t.Run("a certificate for another name is a TLS error", func(t *testing.T) {
		o := &oauthTunnel{}
		tunnel := startOAuthTunnel(t, o)

		res := r.remoteTestOAuth(t, strings.Replace(tunnel.URL, "127.0.0.1", "localhost", 1))

		if got := outcomeOf(res); got != (outcome{Outcome: admin.RemoteTLSError}) {
			t.Errorf("outcome = %+v, want tls_error", got)
		}
	})
}

// reorderedChallenge answers as Wayfindr's challenge does with the
// parameters in another order, a lower-case scheme, and a quoted value with
// an escape in an unrelated parameter.
func (o *oauthTunnel) reorderedChallenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `bearer error_description="a \"b\", c", resource_metadata="`+o.url()+prmPath+`", error="invalid_request"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// TestRemoteConnectionWithOAuthOn: the token's test against a Wayfindr that
// has OAuth on. A bad token gets Wayfindr's 401 invalid_token challenge, not
// a 404, and a request with no token gets the challenge with no error.
func TestRemoteConnectionWithOAuthOn(t *testing.T) {
	anchor := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(anchor.Close)

	h := newHost(t)
	opts := h.options(t)
	opts.RemoteTestClient = trustingClient(anchor)
	opts.RemoteTestTimeout = 400 * time.Millisecond
	r := serve(t, h, opts, tokenConfig(t))
	token := createTokens(t, h, r)["grok"]

	// start serves handler, which is given the tunnel's own address.
	start := func(t *testing.T, build func(url func() string) http.Handler) *httptest.Server {
		t.Helper()

		var srv *httptest.Server

		srv = httptest.NewUnstartedServer(build(func() string { return srv.URL }))
		srv.StartTLS()
		t.Cleanup(srv.Close)

		return srv
	}

	ours := func(url func() string, extra string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+url()+prmPath+`"`+extra)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}

	tests := []struct {
		name  string
		build func(url func() string) http.Handler
		want  outcome
	}{
		{
			name: "a remote token that works and a tokenless request that gets our challenge: ok",
			build: func(url func() string) http.Handler {
				return &fakeMCP{bare: ours(url, "")}
			},
			want: outcome{Outcome: admin.RemoteOK, OK: true, Connectors: 1, Tools: 2},
		},
		{
			name: "a bad token gets our invalid_token challenge: token_invalid",
			build: func(url func() string) http.Handler {
				return byAuth(ours(url, `, error="invalid_token"`), ours(url, ""))
			},
			want: outcome{Outcome: admin.RemoteTokenInvalid, Status: http.StatusUnauthorized},
		},
		{
			name: "token_invalid stands over a bare 404",
			build: func(url func() string) http.Handler {
				return byAuth(ours(url, `, error="invalid_token"`), notFound)
			},
			want: outcome{Outcome: admin.RemoteTokenInvalid, Status: http.StatusUnauthorized},
		},
		{
			name: "token_invalid with the tokenless request treated as local: the proxy warning outranks it",
			build: func(url func() string) http.Handler {
				return byAuth(ours(url, `, error="invalid_token"`), localChallenge)
			},
			want: outcome{Outcome: admin.RemoteProxyTreatsRemoteAsLocal, Status: http.StatusUnauthorized},
		},
		{
			name: "a 401 for the token with a proxy's realm is not ours: token_refused",
			build: func(func() string) http.Handler {
				return byAuth(realmChallenge, realmChallenge)
			},
			want: outcome{Outcome: admin.RemoteTokenRefused, Status: http.StatusUnauthorized},
		},
		{
			name: "a 401 for the token naming another host's metadata is not ours: token_refused",
			build: func(func() string) http.Handler {
				return byAuth(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://elsewhere.example/.well-known/oauth-protected-resource/mcp"`)
					http.Error(w, "unauthorized", http.StatusUnauthorized)
				}, notFound)
			},
			want: outcome{Outcome: admin.RemoteTokenRefused, Status: http.StatusUnauthorized},
		},
		{
			name: "the token works and the tokenless request gets a realm 401: unexpected, not ok",
			build: func(func() string) http.Handler {
				return &fakeMCP{bare: realmChallenge}
			},
			want: outcome{Outcome: admin.RemoteUnexpected, Status: http.StatusUnauthorized},
		},
		{
			name: "a 404 for the token is the path not reaching Wayfindr: not_found",
			build: func(func() string) http.Handler {
				return http.HandlerFunc(notFound)
			},
			want: outcome{Outcome: admin.RemoteNotFound, Status: http.StatusNotFound},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tunnel := start(t, tt.build)
			res := r.remoteTest(t, tunnel.URL, token)

			if diff := cmp.Diff(tt.want, outcomeOf(res)); diff != "" {
				t.Errorf("outcome (-want +got):\n%s\nresult: %+v", diff, res)
			}

			if strings.Contains(res.Message+res.Action, token) {
				t.Errorf("the result repeats the token: %+v", res)
			}
		})
	}

	t.Run("token_invalid is in plain words", func(t *testing.T) {
		tunnel := start(t, func(url func() string) http.Handler {
			return byAuth(ours(url, `, error="invalid_token"`), ours(url, ""))
		})

		res := r.remoteTest(t, tunnel.URL, token)

		for _, want := range []string{"401", "does not accept this token", "revoked"} {
			if !strings.Contains(res.Message, want) {
				t.Errorf("message %q lacks %q", res.Message, want)
			}
		}
	})
}
