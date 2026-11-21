package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// authed serves h as httpd serves the gateway: behind the SDK's bearer
// middleware, which puts the Client the token resolves to into every
// request's TokenInfo. A request with no Authorization header uses the
// default token, so tests that don't care which client calls see every
// connector. A scoped token is "<name>:<connector>,<connector>…".
func authed(h http.Handler) http.Handler {
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if token == clienttoken.DefaultName {
			return clienttoken.Default().TokenInfo(), nil
		}

		name, connectors, ok := strings.Cut(token, ":")

		if !ok {
			return nil, auth.ErrInvalidToken
		}

		c := clienttoken.Client{Name: name, Connectors: []string{}}

		if connectors != "" {
			c.Connectors = strings.Split(connectors, ",")
		}

		return c.TokenInfo(), nil
	}
	h = auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(h)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+clienttoken.DefaultName)
		}

		h.ServeHTTP(w, r)
	})
}

// scopedToken is the bearer token authed resolves to a scoped Client
// called nightly that may use connectors.
func scopedToken(connectors ...string) http.Header {
	return http.Header{"Authorization": {"Bearer nightly:" + strings.Join(connectors, ",")}}
}

// withHeaders is h plus the headers of more, every value of each.
func withHeaders(h http.Header, more http.Header) http.Header {
	out := h.Clone()

	for k, vs := range more {
		out[k] = append(out[k], vs...)
	}

	return out
}

// listIssues is the github fake's tool.
func listIssues() *mcp.Tool {
	return &mcp.Tool{
		Name:        "list_issues",
		Description: "Lists GitHub issues.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}
}

// githubAndGmail is an env with a github and a gmail connector, both up.
func githubAndGmail(t *testing.T, opts gateway.Options) env {
	t.Helper()

	return newEnvWith(t, envOptions{
		fakes: map[string][]fakeupstream.Option{
			"github": {fakeupstream.WithTools(listIssues())},
			"gmail":  {gmailTools()},
		},
		gateway: opts,
	})
}

// everyName is the qualified names search_tools returns over queries.
func everyName(t *testing.T, cs *mcp.ClientSession, queries ...string) []string {
	t.Helper()

	var names []string

	for _, q := range queries {
		names = append(names, searchNames(t, cs, q)...)
	}

	return names
}

var anyToolQueries = []string{"thread", "send message", "purge", "issues", "list github issues"}

// namesOn is the names on connector.
func namesOn(names []string, connector string) []string {
	var out []string

	for _, n := range names {
		if strings.HasPrefix(n, connector+".") {
			out = append(out, n)
		}
	}

	return out
}

// TestScopedTokenSeesOnlyItsConnectors: to a client whose token allows
// only github, gmail is not configured. Search never returns its tools,
// the connectors argument refuses it without naming it as configured, and
// call_tool refuses its tools as an unconfigured connector's, even a tool
// the policy and the client's own profile hide by name, so no hidden_by_*
// Case gives gmail away.
func TestScopedTokenSeesOnlyItsConnectors(t *testing.T) {
	rec := &recorder{}
	e := githubAndGmail(t, gateway.Options{
		Calls: rec,
		Policy: func(c string) config.ToolPolicy {
			if c == "gmail" {
				return config.ToolPolicy{"!purge"}
			}

			return nil
		},
		Profile: profiles(map[string]*config.Scope{
			"acme": {Tools: map[string]config.ToolPolicy{"gmail": {"!send_message"}}},
		}),
	})

	for _, version := range []string{fakeupstream.V20250618, fakeupstream.V20251125, fakeupstream.V20260728} {
		for by, h := range map[string]http.Header{
			"no profile": scopedToken("github"),
			"profile":    withHeaders(scopedToken("github"), http.Header{config.ProfileHeader: {"acme"}}),
		} {
			t.Run(version+"/"+by, func(t *testing.T) {
				cs := connectHeaders(t, e.url, version, h)
				names := everyName(t, cs, anyToolQueries...)

				if got := namesOn(names, "gmail"); len(got) != 0 {
					t.Errorf("search_tools returned %v, want no gmail tool", got)
				}

				if !slices.Contains(names, "github.list_issues") {
					t.Errorf("search_tools returned %v, want github.list_issues", names)
				}

				res := call(t, cs, "search_tools", map[string]any{"query": "thread", "connectors": []string{"gmail"}})
				got := text(t, res)

				if want := "No connector is called gmail. Configured connectors: github."; !res.IsError || !strings.Contains(got, want) {
					t.Errorf("search_tools connectors=[gmail]: (isError %v) %q, want %q", res.IsError, got, want)
				}

				res = call(t, cs, "search_tools", map[string]any{"names": []string{"gmail.get_thread", "github.list_issues"}})

				var page struct {
					Missing []string `json:"missing"`
				}

				if err := json.Unmarshal([]byte(text(t, res)), &page); err != nil {
					t.Fatalf("search_tools names: %v", err)
				}

				if diff := cmp.Diff([]string{"gmail.get_thread"}, page.Missing); diff != "" {
					t.Errorf("names lookup missing (-want +got):\n%s", diff)
				}

				rec.take()

				for _, tool := range []string{"gmail.get_thread", "gmail.send_message", "gmail.purge", "gmail.nope"} {
					wantRefused(t, cs, rec, tool, "unknown_tool", "no_connector",
						"There is no tool called "+tool+": no connector is called gmail.")
				}

				wantCallText(t, cs, "github.list_issues", "")
				rec.take()
			})
		}
	}
}

// TestScopedTokenIsNotToldAnOutOfScopeConnectorIsDown: unavailable lists
// only the token's connectors, so a scoped client isn't told gmail exists
// by its being down, in a search or in a names lookup's advice.
func TestScopedTokenIsNotToldAnOutOfScopeConnectorIsDown(t *testing.T) {
	e := newEnv(t, map[string][]fakeupstream.Option{"github": {fakeupstream.WithTools(listIssues())}}, "gmail")

	unavailable := func(cs *mcp.ClientSession) []string {
		var page struct {
			Unavailable []gateway.Unavailable `json:"unavailable"`
		}

		res := call(t, cs, "search_tools", map[string]any{"query": "issues"})

		if err := json.Unmarshal([]byte(text(t, res)), &page); err != nil {
			t.Fatalf("search_tools: %v", err)
		}

		var out []string

		for _, u := range page.Unavailable {
			out = append(out, u.Connector)
		}

		return out
	}

	if diff := cmp.Diff([]string{"gmail"}, unavailable(connect(t, e.url, fakeupstream.V20251125))); diff != "" {
		t.Errorf("default client: unavailable (-want +got):\n%s", diff)
	}

	cs := connectHeaders(t, e.url, fakeupstream.V20251125, scopedToken("github"))

	if got := unavailable(cs); len(got) != 0 {
		t.Errorf("scoped client: unavailable = %v, want none", got)
	}

	res := call(t, cs, "search_tools", map[string]any{"names": []string{"gmail.get_thread"}})

	if got := text(t, res); !res.IsError || strings.Contains(got, "unreachable") || strings.Contains(got, "is down") {
		t.Errorf("scoped names lookup of gmail: (isError %v) %q, want none found without gmail's state", res.IsError, got)
	}
}

// TestDefaultTokenSeesEveryConnector: the default token narrows nothing.
func TestDefaultTokenSeesEveryConnector(t *testing.T) {
	e := githubAndGmail(t, gateway.Options{})
	cs := connect(t, e.url, fakeupstream.V20251125)
	names := everyName(t, cs, anyToolQueries...)

	for _, want := range []string{"github.list_issues", "gmail.get_thread"} {
		if !slices.Contains(names, want) {
			t.Errorf("search_tools returned %v, want %s", names, want)
		}
	}

	wantCallText(t, cs, "gmail.get_thread", "")
	wantCallText(t, cs, "github.list_issues", "")
}

// TestScopedTokenAndProfileAreANDed: the profile narrows within the token
// and can't widen it. A connector the token allows and the profile leaves
// out is hidden by the profile, which the client chose; one the profile
// allows and the token leaves out is not configured.
func TestScopedTokenAndProfileAreANDed(t *testing.T) {
	rec := &recorder{}
	e := newEnvWith(t, envOptions{
		fakes: map[string][]fakeupstream.Option{
			"github": {fakeupstream.WithTools(listIssues())},
			"gmail":  {gmailTools()},
			"linear": {fakeupstream.WithTools(createIssue())},
		},
		gateway: gateway.Options{Calls: rec, Profile: profiles(map[string]*config.Scope{
			"acme": {Connectors: []string{"linear", "gmail"}},
		})},
	})
	cs := connectHeaders(t, e.url, fakeupstream.V20251125,
		withHeaders(scopedToken("github", "linear"), http.Header{config.ProfileHeader: {"acme"}}))

	names := everyName(t, cs, append(anyToolQueries, "create issue")...)

	if diff := cmp.Diff([]string{"linear.create_issue"}, slices.Compact(slices.Sorted(slices.Values(names)))); diff != "" {
		t.Errorf("search_tools (-want +got):\n%s", diff)
	}

	res := call(t, cs, "search_tools", map[string]any{"query": "thread", "connectors": []string{"gmail"}})

	if want := "Configured connectors: linear."; !res.IsError || !strings.Contains(text(t, res), want) {
		t.Errorf("search_tools connectors=[gmail]: (isError %v) %q, want %q", res.IsError, text(t, res), want)
	}

	if res := call(t, cs, "call_tool", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "x"}}); res.IsError {
		t.Errorf("linear.create_issue: isError result %q, want success", text(t, res))
	}

	rec.take()
	wantRefused(t, cs, rec, "github.list_issues", "unknown_tool", "hidden_by_profile", profileHiddenText)
	wantRefused(t, cs, rec, "gmail.get_thread", "unknown_tool", "no_connector", "no connector is called gmail.")
}

// TestRequestWithoutAClientIsRefused: a request that reaches the gateway
// without a Client in its TokenInfo, as one would if httpd's bearer
// middleware were not wired, is refused rather than served unscoped.
func TestRequestWithoutAClientIsRefused(t *testing.T) {
	rec := &recorder{}
	g := gateway.New(gateway.Options{
		Upstreams: &stubUpstreams{statuses: []upstream.ConnectorStatus{ready()}},
		Catalog:   &listCatalog{tools: []store.Tool{linearTool(t, createIssue())}},
		Calls:     rec,
		Logger:    zerolog.Nop(),
	})
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	cs := connect(t, srv.URL, fakeupstream.V20250618)

	const want = "could not tell which client token this request used"

	if res := call(t, cs, "search_tools", map[string]any{"query": "issue"}); !res.IsError || !strings.Contains(text(t, res), want) {
		t.Errorf("search_tools: (isError %v) %q, want %q", res.IsError, text(t, res), want)
	}

	res := call(t, cs, "call_tool", map[string]any{"name": "linear.create_issue", "arguments": map[string]any{"title": "x"}})

	if !res.IsError || !strings.Contains(text(t, res), want) {
		t.Errorf("call_tool: (isError %v) %q, want %q", res.IsError, text(t, res), want)
	}

	calls := rec.take()

	if len(calls) != 1 || calls[0].ErrorKind != "internal" || calls[0].ErrorCase != "no_client" {
		t.Errorf("call log = %+v, want one internal/no_client call", calls)
	}
}
