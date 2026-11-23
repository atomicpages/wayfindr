package admin_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
)

func TestOAuthSettings(t *testing.T) {
	st, user := newStore(t)

	tests := []struct {
		name  string
		state admin.ConfigState
		want  admin.OAuthSettings
	}{
		{
			name:  "off, no public URL",
			state: admin.ConfigState{},
			want:  admin.OAuthSettings{Clients: []string{}},
		},
		{
			name: "on, issuer has no trailing slash",
			state: admin.ConfigState{
				PublicURL: "https://wayfindr.example.com/",
				OAuth:     config.OAuth{Enabled: true, Clients: []string{config.DefaultOAuthClient}},
			},
			want: admin.OAuthSettings{Enabled: true, Clients: []string{config.DefaultOAuthClient}, Issuer: "https://wayfindr.example.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, tt.state))
			got, err := c.OAuthSettings(context.Background())

			if err != nil {
				t.Fatalf("OAuthSettings: %v", err)
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("OAuthSettings (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSetOAuthEnabled(t *testing.T) {
	st, user := newStore(t)

	t.Run("hands the flag to the daemon and answers the running settings", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})

		running := config.OAuth{Clients: []string{config.DefaultOAuthClient}}
		opts.Config = func() admin.ConfigState {
			return admin.ConfigState{PublicURL: "https://wayfindr.example.com", OAuth: running}
		}

		var got []bool

		opts.SetOAuthEnabled = func(_ context.Context, enabled bool) error {
			got = append(got, enabled)
			running.Enabled = enabled

			return nil
		}

		c, _ := serve(t, opts)

		for _, enabled := range []bool{true, false} {
			res, err := c.SetOAuthEnabled(context.Background(), enabled)

			if err != nil {
				t.Fatalf("SetOAuthEnabled(%v): %v", enabled, err)
			}

			want := admin.OAuthSettings{Enabled: enabled, Clients: []string{config.DefaultOAuthClient}, Issuer: "https://wayfindr.example.com"}

			if diff := cmp.Diff(want, res); diff != "" {
				t.Errorf("SetOAuthEnabled(%v) (-want +got):\n%s", enabled, diff)
			}
		}

		if diff := cmp.Diff([]bool{true, false}, got); diff != "" {
			t.Errorf("flags the daemon got (-want +got):\n%s", diff)
		}
	})

	refusals := []struct {
		name   string
		err    error
		status int
		text   string
	}{
		{
			"invalid_oauth is 400",
			&admin.ConfigError{Kind: admin.ConfigKindInvalid, Case: admin.ConfigCaseInvalidOAuth, Advice: admin.Advice{Summary: "Set the public URL first."}},
			http.StatusBadRequest, "Set the public URL first.",
		},
		{
			"oauth_not_table is 409",
			&admin.ConfigError{Kind: admin.ConfigKindUnsafe, Case: admin.ConfigCaseOAuthNotTable, Advice: admin.Advice{Summary: "Edit the file by hand."}},
			http.StatusConflict, "Edit the file by hand.",
		},
		{
			"config_changed is 409",
			&admin.ConfigError{Kind: admin.ConfigKindConflict, Case: admin.ConfigCaseConfigChanged, Advice: admin.Advice{Summary: "Try again."}},
			http.StatusConflict, "Try again.",
		},
		{"an unexpected error is 500 without its text", io.ErrUnexpectedEOF, http.StatusInternalServerError, ""},
	}

	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
			opts.SetOAuthEnabled = func(context.Context, bool) error { return tt.err }
			c, _ := serve(t, opts)
			_, err := c.SetOAuthEnabled(context.Background(), true)

			if got := statusOf(err); got != tt.status {
				t.Fatalf("status = %d (%v), want %d", got, err, tt.status)
			}

			if tt.text != "" && !strings.Contains(err.Error(), tt.text) {
				t.Errorf("err = %v, want the advice %q", err, tt.text)
			}

			if tt.text == "" && strings.Contains(err.Error(), "unexpected EOF") {
				t.Errorf("err = %v, want the cause hidden", err)
			}
		})
	}

	t.Run("not wired is 501", func(t *testing.T) {
		c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))
		_, err := c.SetOAuthEnabled(context.Background(), true)

		if got := statusOf(err); got != http.StatusNotImplemented {
			t.Errorf("status = %d (%v), want 501", got, err)
		}
	})

	t.Run("a malformed body is 400 and never reaches the daemon", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
		opts.SetOAuthEnabled = func(context.Context, bool) error {
			t.Error("SetOAuthEnabled called for a malformed body")

			return nil
		}
		_, srv := serve(t, opts)

		for _, body := range []string{``, `{}`, `{"enabled":null}`, `{"enabled":"yes"}`, `{"enabled":true,"extra":1}`, `true`, `{"enabled":true}{}`} {
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, srv.URL+"/admin/server/oauth", strings.NewReader(body))
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
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
		opts.SetOAuthEnabled = func(context.Context, bool) error {
			t.Error("SetOAuthEnabled called for an oversized body")

			return nil
		}
		_, srv := serve(t, opts)
		body := `{"enabled":true,"pad":"` + strings.Repeat("a", admin.MaxOAuthSettingsBody) + `"}`
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, srv.URL+"/admin/server/oauth", strings.NewReader(body))
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
