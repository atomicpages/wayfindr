package gateway_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

const (
	scopeHiddenText  = "is hidden by this project's Wayfindr scope"
	scopeInvalidText = "scope (.wayfindr.toml) is invalid"
)

// withHeader adds its headers, every value of each, to every request, as
// the stdio shim adds the project scope and the profile.
type withHeader http.Header

func (h withHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())

	for name, vals := range h {
		for _, v := range vals {
			r.Header.Add(name, v)
		}
	}

	return http.DefaultTransport.RoundTrip(r)
}

// connectScoped is connect with header value sent as config.ScopeHeader on
// every request.
func connectScoped(t *testing.T, url, version, header string) *mcp.ClientSession {
	t.Helper()

	return connectHeaders(t, url, version, http.Header{config.ScopeHeader: {header}})
}

// connectHeaders is connect with h sent on every request.
func connectHeaders(t *testing.T, url, version string, h http.Header) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   url,
		MaxRetries: -1,
		HTTPClient: &http.Client{Transport: withHeader(h)},
	}, &mcp.ClientSessionOptions{ProtocolVersion: version})

	if err != nil {
		t.Fatalf("Connect at %s: %v", version, err)
	}

	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

func scopeHeader(t *testing.T, s *config.Scope) string {
	t.Helper()
	h, err := s.Header()

	if err != nil {
		t.Fatal(err)
	}

	return h
}

// TestScopeNarrowsSearchAndCalls: a scoped request sees only the connectors
// the scope allows, and of those only the tools both the config and the
// scope expose. Out-of-scope connectors are not configured as far as it can
// tell, and calls to anything hidden are refused as hidden by the scope.
func TestScopeNarrowsSearchAndCalls(t *testing.T) {
	e := newEnvWith(t, envOptions{fakes: map[string][]fakeupstream.Option{
		"gmail":  {gmailTools()},
		"linear": {fakeupstream.WithTools(createIssue())},
	}})

	for _, version := range []string{fakeupstream.V20251125, fakeupstream.V20260728} {
		t.Run(version, func(t *testing.T) {
			cs := connectScoped(t, e.url, version, scopeHeader(t, &config.Scope{
				Connectors: []string{"gmail"},
				Tools:      map[string]config.ToolPolicy{"gmail": {"@read"}},
			}))

			if got := searchNames(t, cs, "get thread"); !slices.Contains(got, "gmail.get_thread") {
				t.Errorf("search_tools = %v, want gmail.get_thread", got)
			}

			if got := searchNames(t, cs, "send message"); slices.Contains(got, "gmail.send_message") {
				t.Errorf("search_tools = %v, want no gmail.send_message, which the scope hides", got)
			}

			if got := searchNames(t, cs, "create issue"); len(got) != 0 {
				t.Errorf("search_tools = %v, want nothing from linear, which the scope leaves out", got)
			}

			res := call(t, cs, "search_tools", map[string]any{"query": "issue", "connectors": []string{"linear"}})

			if got := text(t, res); !res.IsError || !strings.Contains(got, "No connector is called linear") {
				t.Errorf("search_tools connectors=[linear]: (isError %v) %q, want unknown connector", res.IsError, got)
			}

			var page struct {
				Missing []string `json:"missing"`
			}

			res = call(t, cs, "search_tools", map[string]any{"names": []string{"gmail.get_thread", "gmail.send_message", "linear.create_issue"}})

			if err := json.Unmarshal([]byte(text(t, res)), &page); err != nil {
				t.Fatalf("search_tools names: %v", err)
			}

			if diff := cmp.Diff([]string{"gmail.send_message", "linear.create_issue"}, page.Missing); diff != "" {
				t.Errorf("names lookup missing (-want +got):\n%s", diff)
			}

			wantCallText(t, cs, "gmail.get_thread", "")
			wantCallText(t, cs, "gmail.send_message", scopeHiddenText)
			wantCallText(t, cs, "linear.create_issue", scopeHiddenText)
		})
	}
}

// TestScopeWithNoConnectorsFindsNothing: an empty allowlist must not reach
// the store as "no filter".
func TestScopeWithNoConnectorsFindsNothing(t *testing.T) {
	e := newEnvWith(t, envOptions{fakes: map[string][]fakeupstream.Option{"gmail": {gmailTools()}}})
	cs := connectScoped(t, e.url, fakeupstream.V20251125, scopeHeader(t, &config.Scope{Connectors: []string{}}))

	if got := searchNames(t, cs, "get thread"); len(got) != 0 {
		t.Errorf("search_tools = %v, want nothing", got)
	}

	wantCallText(t, cs, "gmail.get_thread", scopeHiddenText)
}

// TestScopeHeaderThatDoesNotParseIsRefused: a broken scope must not be
// read as no scope, which would widen what the project gets.
func TestScopeHeaderThatDoesNotParseIsRefused(t *testing.T) {
	e := newEnvWith(t, envOptions{fakes: map[string][]fakeupstream.Option{"gmail": {gmailTools()}}})

	for _, h := range []string{"{", `{"connectors":["gmail"],"allow":["*"]}`} {
		cs := connectScoped(t, e.url, fakeupstream.V20251125, h)
		res := call(t, cs, "search_tools", map[string]any{"query": "thread"})

		if got := text(t, res); !res.IsError || !strings.Contains(got, scopeInvalidText) {
			t.Errorf("header %q: search_tools (isError %v) %q, want %q", h, res.IsError, got, scopeInvalidText)
		}

		wantCallText(t, cs, "gmail.get_thread", scopeInvalidText)
	}
}

const (
	profileHiddenText  = "is hidden by the Wayfindr profile acme."
	profileAllowText   = "allow it in [profiles.acme] in the config"
	badProfileText     = "names its Wayfindr profile more than once, or with a name that isn't valid"
	unknownProfileText = "asks for the Wayfindr profile acme2, which the config doesn't define"
)

// profiles is an Options.Profile over a fixed set of profiles.
func profiles(defined map[string]*config.Scope) func(string) (*config.Scope, bool) {
	return func(name string) (*config.Scope, bool) {
		p, ok := defined[name]

		return p, ok
	}
}

// wantRefused calls tool through call_tool and checks that it is refused
// with advice containing each of want, and that the call log records it as
// rejected with kind and errCase.
func wantRefused(t *testing.T, cs *mcp.ClientSession, rec *recorder, tool, kind, errCase string, want ...string) {
	t.Helper()
	res := call(t, cs, "call_tool", map[string]any{"name": tool})
	got := text(t, res)

	for _, w := range want {
		if !res.IsError || !strings.Contains(got, w) {
			t.Errorf("%s: result (isError %v) %q, want an error containing %q", tool, res.IsError, got, w)
		}
	}

	calls := rec.take()

	if len(calls) != 1 {
		t.Fatalf("%s: recorded %d calls, want 1", tool, len(calls))
	}

	type logged struct{ Result, Kind, Case string }

	if diff := cmp.Diff(logged{string(store.CallRejected), kind, errCase},
		logged{string(calls[0].Result), calls[0].ErrorKind, calls[0].ErrorCase}); diff != "" {
		t.Errorf("%s: call log (-want +got):\n%s", tool, diff)
	}
}

// TestProfileNarrowsSearchAndCalls: a profile, named by header as the stdio
// shim sends it or by ?profile= in the endpoint URL, narrows a request as a
// project scope does, and a refusal names the profile and where to change
// it.
func TestProfileNarrowsSearchAndCalls(t *testing.T) {
	rec := &recorder{}
	e := newEnvWith(t, envOptions{
		fakes: map[string][]fakeupstream.Option{
			"gmail":  {gmailTools()},
			"linear": {fakeupstream.WithTools(createIssue())},
		},
		gateway: gateway.Options{Calls: rec, Profile: profiles(map[string]*config.Scope{
			"acme": {Connectors: []string{"gmail"}, Tools: map[string]config.ToolPolicy{"gmail": {"@read"}}},
		})},
	})

	for _, version := range []string{fakeupstream.V20251125, fakeupstream.V20260728} {
		connects := map[string]func(t *testing.T) *mcp.ClientSession{
			"header": func(t *testing.T) *mcp.ClientSession {
				return connectHeaders(t, e.url, version, http.Header{config.ProfileHeader: {"acme"}})
			},
			"query": func(t *testing.T) *mcp.ClientSession {
				return connectHeaders(t, e.url+"?profile=acme", version, nil)
			},
		}

		for by, connectTo := range connects {
			t.Run(version+"/"+by, func(t *testing.T) {
				cs := connectTo(t)

				if got := searchNames(t, cs, "get thread"); !slices.Contains(got, "gmail.get_thread") {
					t.Errorf("search_tools = %v, want gmail.get_thread", got)
				}

				if got := searchNames(t, cs, "send message"); slices.Contains(got, "gmail.send_message") {
					t.Errorf("search_tools = %v, want no gmail.send_message, which the profile hides", got)
				}

				if got := searchNames(t, cs, "create issue"); len(got) != 0 {
					t.Errorf("search_tools = %v, want nothing from linear, which the profile leaves out", got)
				}

				var page struct {
					Missing []string `json:"missing"`
				}

				res := call(t, cs, "search_tools", map[string]any{"names": []string{"gmail.get_thread", "gmail.send_message", "linear.create_issue"}})

				if err := json.Unmarshal([]byte(text(t, res)), &page); err != nil {
					t.Fatalf("search_tools names: %v", err)
				}

				if diff := cmp.Diff([]string{"gmail.send_message", "linear.create_issue"}, page.Missing); diff != "" {
					t.Errorf("names lookup missing (-want +got):\n%s", diff)
				}

				wantCallText(t, cs, "gmail.get_thread", "")
				rec.take()
				// Hidden by class, after the catalog is read, and by
				// connector, before it.
				wantRefused(t, cs, rec, "gmail.send_message", "unknown_tool", "hidden_by_profile",
					"gmail.send_message "+profileHiddenText, profileAllowText)
				wantRefused(t, cs, rec, "linear.create_issue", "unknown_tool", "hidden_by_profile",
					"linear.create_issue "+profileHiddenText, profileAllowText)
			})
		}
	}
}

// TestProfileAndScopeAreANDed: a tool must pass both the profile and the
// project scope, and the refusal names whichever hid it, the profile first.
func TestProfileAndScopeAreANDed(t *testing.T) {
	tests := []struct {
		name          string
		profile, file *config.Scope
		// want maps each tool to the Case that refuses it; "" is called.
		want map[string]string
	}{
		{
			name:    "profile leaves out a connector the file allows",
			profile: &config.Scope{Connectors: []string{"gmail"}, Tools: map[string]config.ToolPolicy{"gmail": {"!purge"}}},
			file:    &config.Scope{Connectors: []string{"gmail", "linear"}, Tools: map[string]config.ToolPolicy{"gmail": {"!send_message"}}},
			want: map[string]string{
				"gmail.get_thread":    "",
				"gmail.purge":         "hidden_by_profile",
				"gmail.send_message":  "hidden_by_scope",
				"linear.create_issue": "hidden_by_profile",
			},
		},
		{
			name:    "file leaves out a connector the profile allows",
			profile: &config.Scope{Connectors: []string{"gmail", "linear"}},
			file:    &config.Scope{Connectors: []string{"gmail"}},
			want: map[string]string{
				"gmail.get_thread":    "",
				"linear.create_issue": "hidden_by_scope",
			},
		},
		{
			// Checked before the catalog is read.
			name:    "both hide it by name",
			profile: &config.Scope{Tools: map[string]config.ToolPolicy{"gmail": {"!send_message"}}},
			file:    &config.Scope{Tools: map[string]config.ToolPolicy{"gmail": {"!send_message"}}},
			want: map[string]string{
				"gmail.get_thread":   "",
				"gmail.send_message": "hidden_by_profile",
			},
		},
		{
			// Checked after the catalog is read.
			name:    "both hide it by class",
			profile: &config.Scope{Tools: map[string]config.ToolPolicy{"gmail": {"@read"}}},
			file:    &config.Scope{Tools: map[string]config.ToolPolicy{"gmail": {"@read"}}},
			want: map[string]string{
				"gmail.get_thread":   "",
				"gmail.send_message": "hidden_by_profile",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			e := newEnvWith(t, envOptions{
				fakes: map[string][]fakeupstream.Option{
					"gmail":  {gmailTools()},
					"linear": {fakeupstream.WithTools(createIssue())},
				},
				gateway: gateway.Options{Calls: rec, Profile: profiles(map[string]*config.Scope{"acme": tt.profile})},
			})
			cs := connectHeaders(t, e.url, fakeupstream.V20251125, http.Header{
				config.ProfileHeader: {"acme"},
				config.ScopeHeader:   {scopeHeader(t, tt.file)},
			})

			_, linear := tt.want["linear.create_issue"]

			if got := searchNames(t, cs, "create issue"); linear && len(got) != 0 {
				t.Errorf("search_tools = %v, want nothing from linear, which one of them leaves out", got)
			}

			if c, ok := tt.want["gmail.send_message"]; ok {
				if got := searchNames(t, cs, "send message"); slices.Contains(got, "gmail.send_message") != (c == "") {
					t.Errorf("search_tools = %v, want gmail.send_message only if neither hides it", got)
				}
			}

			for _, tool := range slices.Sorted(maps.Keys(tt.want)) {
				switch c := tt.want[tool]; c {
				case "":
					wantCallText(t, cs, tool, "")
					rec.take()
				case "hidden_by_profile":
					wantRefused(t, cs, rec, tool, "unknown_tool", c, tool+" "+profileHiddenText, profileAllowText)
				default:
					wantRefused(t, cs, rec, tool, "unknown_tool", c, tool+" "+scopeHiddenText)
				}
			}
		})
	}
}

// TestUnknownProfileIsRefused: a name the config doesn't define refuses
// every request rather than serving it unscoped, and the advice names it.
func TestUnknownProfileIsRefused(t *testing.T) {
	for name, profile := range map[string]func(string) (*config.Scope, bool){
		"profiles defined": profiles(map[string]*config.Scope{"acme": {}}),
		"no profiles":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			e := newEnvWith(t, envOptions{
				fakes:   map[string][]fakeupstream.Option{"gmail": {gmailTools()}},
				gateway: gateway.Options{Calls: rec, Profile: profile},
			})
			cs := connectHeaders(t, e.url+"?profile=acme2", fakeupstream.V20251125, nil)
			res := call(t, cs, "search_tools", map[string]any{"query": "thread"})

			if got := text(t, res); !res.IsError || !strings.Contains(got, unknownProfileText) || !strings.Contains(got, "wayfindr scope --profile acme2") {
				t.Errorf("search_tools (isError %v) %q, want %q", res.IsError, got, unknownProfileText)
			}

			wantRefused(t, cs, rec, "gmail.get_thread", "unknown_profile", "", unknownProfileText)
		})
	}
}

// TestBadProfileIsRefused: a profile named twice, by header and query or
// by a repeated query, or with an invalid name, is refused rather than one
// of them chosen, and the name is not echoed back.
func TestBadProfileIsRefused(t *testing.T) {
	tests := []struct {
		name  string
		query string
		h     http.Header
	}{
		{"header and query", "?profile=acme", http.Header{config.ProfileHeader: {"acme"}}},
		{"query twice", "?profile=acme&profile=acme", nil},
		{"header twice", "", http.Header{config.ProfileHeader: {"acme", "beta"}}},
		{"invalid name in header", "", http.Header{config.ProfileHeader: {"EvilName"}}},
		{"invalid name in query", "?profile=Evil%20Name%3B", nil},
		{"empty name", "?profile=", nil},
		// A query that doesn't parse would drop its profile, so it is
		// refused rather than served unscoped; so is a misspelled key.
		{"semicolon in query", "?profile=acme;x", nil},
		{"bad escape in query", "?profile=ac%zzme", nil},
		{"key in another case", "?Profile=acme", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			e := newEnvWith(t, envOptions{
				fakes: map[string][]fakeupstream.Option{"gmail": {gmailTools()}},
				gateway: gateway.Options{Calls: rec, Profile: profiles(map[string]*config.Scope{
					"acme": {}, "beta": {},
				})},
			})
			cs := connectHeaders(t, e.url+tt.query, fakeupstream.V20251125, tt.h)
			res := call(t, cs, "search_tools", map[string]any{"query": "thread"})
			got := text(t, res)

			if !res.IsError || !strings.Contains(got, badProfileText) {
				t.Errorf("search_tools (isError %v) %q, want %q", res.IsError, got, badProfileText)
			}

			if strings.Contains(got, "Evil") {
				t.Errorf("search_tools %q echoes the invalid name", got)
			}

			wantRefused(t, cs, rec, "gmail.get_thread", "invalid_scope", "bad_profile", badProfileText)
		})
	}
}

// TestProfileIsResolvedPerRequest: the gateway asks for the profile on
// every request, so a reload that changes it applies to a connected client
// at once.
func TestProfileIsResolvedPerRequest(t *testing.T) {
	var current atomic.Pointer[config.Scope]

	current.Store(&config.Scope{Connectors: []string{"gmail"}})
	e := newEnvWith(t, envOptions{
		fakes: map[string][]fakeupstream.Option{
			"gmail":  {gmailTools()},
			"linear": {fakeupstream.WithTools(createIssue())},
		},
		gateway: gateway.Options{Profile: func(name string) (*config.Scope, bool) {
			return current.Load(), name == "acme"
		}},
	})
	cs := connectHeaders(t, e.url, fakeupstream.V20251125, http.Header{config.ProfileHeader: {"acme"}})

	if got := searchNames(t, cs, "create issue"); len(got) != 0 {
		t.Errorf("before: search_tools = %v, want nothing from linear", got)
	}

	wantCallText(t, cs, "gmail.get_thread", "")

	current.Store(&config.Scope{Connectors: []string{"linear"}})

	if got := searchNames(t, cs, "create issue"); !slices.Contains(got, "linear.create_issue") {
		t.Errorf("after: search_tools = %v, want linear.create_issue", got)
	}

	wantCallText(t, cs, "gmail.get_thread", profileHiddenText)
}

// TestNoProfileLeavesRequestsUnscoped: with profiles defined, a request
// that names none sees everything, as before profiles.
func TestNoProfileLeavesRequestsUnscoped(t *testing.T) {
	var asked atomic.Bool

	e := newEnvWith(t, envOptions{
		fakes: map[string][]fakeupstream.Option{
			"gmail":  {gmailTools()},
			"linear": {fakeupstream.WithTools(createIssue())},
		},
		gateway: gateway.Options{Profile: func(string) (*config.Scope, bool) {
			asked.Store(true)

			return &config.Scope{Connectors: []string{}}, true
		}},
	})
	cs := connect(t, e.url, fakeupstream.V20251125)

	if got := searchNames(t, cs, "create issue"); !slices.Contains(got, "linear.create_issue") {
		t.Errorf("search_tools = %v, want linear.create_issue", got)
	}

	wantCallText(t, cs, "gmail.send_message", "")

	if asked.Load() {
		t.Error("Options.Profile was asked for a profile no request named")
	}
}

// TestPolicyHidesByNameBeforeTheProfile: a tool both the config's tools
// list and the profile hide by name is refused for the policy, so the
// advice names the list that must change first, as hiddenBy's order does.
func TestPolicyHidesByNameBeforeTheProfile(t *testing.T) {
	rec := &recorder{}
	e := newEnvWith(t, envOptions{
		fakes: map[string][]fakeupstream.Option{"gmail": {gmailTools()}},
		gateway: gateway.Options{
			Calls:  rec,
			Policy: func(string) config.ToolPolicy { return config.ToolPolicy{"!send_message"} },
			Profile: profiles(map[string]*config.Scope{
				"acme": {Tools: map[string]config.ToolPolicy{"gmail": {"!send_message"}}},
			}),
		},
	})
	cs := connectHeaders(t, e.url, fakeupstream.V20251125, http.Header{config.ProfileHeader: {"acme"}})

	wantRefused(t, cs, rec, "gmail.send_message", "unknown_tool", "hidden_by_policy", "[connectors.gmail]")
}
