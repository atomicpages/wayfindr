package admin_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// TestDescribeRemote: every outcome gets a message, a failure gets an action,
// and the facts a person needs (the host, the status) are in the words. The
// daemon's tester and the admin handler both take their words from here.
func TestDescribeRemote(t *testing.T) {
	facts := admin.RemoteFacts{Host: "x.example.com", URL: "https://x.example.com/mcp", Status: 418, Redirect: "login.example.com", Connectors: 1, Tools: 2}

	tests := []struct {
		name    string
		outcome admin.RemoteOutcome
		detail  admin.RemoteDetail
		// contains are phrases the message has.
		contains []string
	}{
		{"ok", admin.RemoteOK, admin.RemoteDetailNone, []string{"https://x.example.com/mcp", "sees 1 connector."}},
		{"no public URL", admin.RemoteNotConfigured, admin.RemoteDetailNone, []string{"No public URL is set"}},
		{"invalid public URL", admin.RemoteNotConfigured, admin.RemoteDetailInvalidURL, []string{"not a valid https address"}},
		{"unreachable", admin.RemoteUnreachable, admin.RemoteDetailNone, []string{"x.example.com"}},
		{"tunnel offline", admin.RemoteUnreachable, admin.RemoteDetailTunnelOffline, []string{"x.example.com", "tunnel", "not running"}},
		{"tls", admin.RemoteTLSError, admin.RemoteDetailNone, []string{"x.example.com"}},
		{"timeout", admin.RemoteTimeout, admin.RemoteDetailNone, []string{"x.example.com"}},
		{"interstitial", admin.RemoteInterstitial, admin.RemoteDetailNone, []string{"x.example.com", "web page"}},
		{"redirect", admin.RemoteRedirect, admin.RemoteDetailNone, []string{"x.example.com", "login.example.com"}},
		{"not found", admin.RemoteNotFound, admin.RemoteDetailNone, []string{"x.example.com", "404", "not a remote token", "wrong", "revoked"}},
		{"token refused", admin.RemoteTokenRefused, admin.RemoteDetailNone, []string{"x.example.com", "HTTP 418", "login or access check"}},
		{"proxy treats remote as local", admin.RemoteProxyTreatsRemoteAsLocal, admin.RemoteDetailNone, []string{"Not safe", "x.example.com", "default client token"}},
		{"unexpected status", admin.RemoteUnexpected, admin.RemoteDetailNone, []string{"x.example.com", "HTTP 418"}},
		{"unexpected, not MCP", admin.RemoteUnexpected, admin.RemoteDetailNotMCP, []string{"x.example.com", "not like Wayfindr's MCP endpoint"}},
		{"unexpected, tokenless status", admin.RemoteUnexpected, admin.RemoteDetailBareStatus, []string{"The token works", "HTTP 418", "x.example.com"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := facts
			f.Detail = tt.detail
			got := admin.DescribeRemote(tt.outcome, f)

			if got.Outcome != tt.outcome || got.OK != (tt.outcome == admin.RemoteOK) {
				t.Errorf("outcome = %q, ok = %v; want %q", got.Outcome, got.OK, tt.outcome)
			}

			for _, want := range tt.contains {
				if !strings.Contains(got.Message, want) {
					t.Errorf("message %q lacks %q", got.Message, want)
				}
			}

			if !got.OK && got.Action == "" {
				t.Errorf("a failure with no action: %+v", got)
			}

			if got.OK && got.Action != "" {
				t.Errorf("a success with an action: %+v", got)
			}
		})
	}

	t.Run("not_configured reports no address and no status", func(t *testing.T) {
		got := admin.DescribeRemote(admin.RemoteNotConfigured, facts)

		if got.URL != "" || got.Status != 0 {
			t.Errorf("result = %+v, want no url and no status", got)
		}
	})

	t.Run("ok carries the counts, and plural words them", func(t *testing.T) {
		got := admin.DescribeRemote(admin.RemoteOK, admin.RemoteFacts{URL: "https://x.example.com/mcp", Connectors: 3, Tools: 2})

		if got.Connectors != 3 || got.Tools != 2 || !strings.Contains(got.Message, "sees 3 connectors.") {
			t.Errorf("result = %+v", got)
		}
	})

	t.Run("a redirect with no known host says another address", func(t *testing.T) {
		got := admin.DescribeRemote(admin.RemoteRedirect, admin.RemoteFacts{Host: "x.example.com"})

		if !strings.Contains(got.Message, "another address") {
			t.Errorf("message = %q", got.Message)
		}
	})
}

func TestTestRemote(t *testing.T) {
	st, user := newStore(t)

	t.Run("hands the public URL and token to the daemon and answers its result", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{PublicURL: "https://wayfindr.example.com"})

		var gotURL, gotToken string

		opts.TestRemote = func(_ context.Context, publicURL, token string) admin.RemoteTestResult {
			gotURL, gotToken = publicURL, token

			return admin.RemoteTestResult{Outcome: admin.RemoteOK, Message: "It works.", URL: publicURL + "/mcp", Connectors: 2, Tools: 2}
		}

		c, _ := serve(t, opts)
		got, err := c.TestRemote(context.Background(), "wf_s_secret")

		if err != nil {
			t.Fatalf("TestRemote: %v", err)
		}

		want := admin.RemoteTestResult{
			Outcome: admin.RemoteOK, OK: true, Message: "It works.",
			URL: "https://wayfindr.example.com/mcp", Connectors: 2, Tools: 2,
		}

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("result (-want +got):\n%s", diff)
		}

		if gotURL != "https://wayfindr.example.com" || gotToken != "wf_s_secret" {
			t.Errorf("daemon got url %q, token %q", gotURL, gotToken)
		}
	})

	t.Run("a failed outcome is 200 with ok false, whatever the daemon set", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{PublicURL: "https://wayfindr.example.com"})
		opts.TestRemote = func(context.Context, string, string) admin.RemoteTestResult {
			return admin.RemoteTestResult{Outcome: admin.RemoteProxyTreatsRemoteAsLocal, OK: true, Message: "Not safe."}
		}

		c, _ := serve(t, opts)
		got, err := c.TestRemote(context.Background(), "wf_s_secret")

		if err != nil {
			t.Fatalf("TestRemote: %v", err)
		}

		if got.OK || got.Outcome != admin.RemoteProxyTreatsRemoteAsLocal {
			t.Errorf("result = %+v, want the proxy outcome with ok false", got)
		}
	})

	t.Run("no public URL is the not_configured outcome and never reaches the daemon", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
		opts.TestRemote = func(context.Context, string, string) admin.RemoteTestResult {
			t.Error("TestRemote called with no public URL")

			return admin.RemoteTestResult{}
		}

		c, _ := serve(t, opts)
		got, err := c.TestRemote(context.Background(), "wf_s_secret")

		if err != nil {
			t.Fatalf("TestRemote: %v", err)
		}

		if got.Outcome != admin.RemoteNotConfigured || got.OK || got.Message == "" || got.Action == "" || got.URL != "" {
			t.Errorf("result = %+v, want not_configured with a message and an action", got)
		}
	})

	t.Run("not wired is 501", func(t *testing.T) {
		c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{PublicURL: "https://wayfindr.example.com"}))
		_, err := c.TestRemote(context.Background(), "wf_s_secret")

		if got := statusOf(err); got != http.StatusNotImplemented {
			t.Errorf("status = %d (%v), want 501", got, err)
		}
	})

	t.Run("a malformed body is 400 and never reaches the daemon", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{PublicURL: "https://wayfindr.example.com"})
		opts.TestRemote = func(context.Context, string, string) admin.RemoteTestResult {
			t.Error("TestRemote called for a malformed body")

			return admin.RemoteTestResult{}
		}
		_, srv := serve(t, opts)

		for _, body := range []string{``, `{}`, `{"token":""}`, `{"token":7}`, `{"token":"x","extra":1}`, `"x"`, `{"token":"x"}{}`} {
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/admin/remote/test", strings.NewReader(body))
			res, err := srv.Client().Do(req)

			if err != nil {
				t.Fatal(err)
			}

			_ = res.Body.Close()

			if res.StatusCode != http.StatusBadRequest {
				t.Errorf("body %q: status %d, want 400", body, res.StatusCode)
			}
		}
	})

	t.Run("an oversized body is 413", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{PublicURL: "https://wayfindr.example.com"})
		opts.TestRemote = func(context.Context, string, string) admin.RemoteTestResult {
			t.Error("TestRemote called for an oversized body")

			return admin.RemoteTestResult{}
		}
		_, srv := serve(t, opts)
		body := `{"token":"` + strings.Repeat("a", admin.MaxRemoteTestBody) + `"}`
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/admin/remote/test", strings.NewReader(body))
		res, err := srv.Client().Do(req)

		if err != nil {
			t.Fatal(err)
		}

		_ = res.Body.Close()

		if res.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("status %d, want 413", res.StatusCode)
		}
	})
}
