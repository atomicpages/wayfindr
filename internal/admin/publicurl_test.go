package admin_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

func TestPublicURL(t *testing.T) {
	st, user := newStore(t)

	for _, running := range []string{"", "https://wayfindr.example.com"} {
		c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{PublicURL: running}))
		got, err := c.PublicURL(context.Background())

		if err != nil {
			t.Fatalf("PublicURL: %v", err)
		}

		if got.URL != running {
			t.Errorf("PublicURL = %q, want the running config's %q", got.URL, running)
		}
	}
}

func TestSetPublicURL(t *testing.T) {
	st, user := newStore(t)

	t.Run("hands the URL to the daemon and answers it", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})

		var got []string

		opts.SetPublicURL = func(_ context.Context, u string) error {
			got = append(got, u)

			return nil
		}

		c, _ := serve(t, opts)

		for _, u := range []string{"https://wayfindr.example.com", ""} {
			res, err := c.SetPublicURL(context.Background(), u)

			if err != nil {
				t.Fatalf("SetPublicURL(%q): %v", u, err)
			}

			if res.URL != u {
				t.Errorf("SetPublicURL(%q) answered %q", u, res.URL)
			}
		}

		if diff := cmp.Diff([]string{"https://wayfindr.example.com", ""}, got); diff != "" {
			t.Errorf("URLs the daemon got (-want +got):\n%s", diff)
		}
	})

	t.Run("a refusal answers its status and advice", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
		opts.SetPublicURL = func(context.Context, string) error {
			return &admin.ConfigError{Kind: admin.ConfigKindInvalid, Case: admin.ConfigCaseInvalidPublicURL,
				Advice: admin.Advice{Summary: "The public URL must be an https URL."}}
		}
		c, _ := serve(t, opts)
		_, err := c.SetPublicURL(context.Background(), "http://x")

		if got := statusOf(err); got != http.StatusBadRequest || !strings.Contains(err.Error(), "must be an https URL") {
			t.Errorf("err = %v (status %d), want 400 with the advice", err, got)
		}
	})

	t.Run("an unexpected error is 500 without its text", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
		opts.SetPublicURL = func(context.Context, string) error { return io.ErrUnexpectedEOF }
		c, _ := serve(t, opts)
		_, err := c.SetPublicURL(context.Background(), "https://x.example.com")

		if got := statusOf(err); got != http.StatusInternalServerError || strings.Contains(err.Error(), "unexpected EOF") {
			t.Errorf("err = %v (status %d), want 500 that hides the cause", err, got)
		}
	})

	t.Run("not wired is 501", func(t *testing.T) {
		c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))
		_, err := c.SetPublicURL(context.Background(), "https://x.example.com")

		if got := statusOf(err); got != http.StatusNotImplemented {
			t.Errorf("status = %d (%v), want 501", got, err)
		}
	})

	t.Run("a malformed body is 400 and never reaches the daemon", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
		opts.SetPublicURL = func(context.Context, string) error {
			t.Error("SetPublicURL called for a malformed body")

			return nil
		}
		_, srv := serve(t, opts)

		for _, body := range []string{``, `{}`, `{"public_url":null}`, `{"public_url":7}`, `{"public_url":"https://x.example.com","extra":1}`, `"https://x.example.com"`, `{"public_url":"https://x.example.com"}{}`} {
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, srv.URL+"/admin/server/public-url", strings.NewReader(body))
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
		opts.SetPublicURL = func(context.Context, string) error {
			t.Error("SetPublicURL called for an oversized body")

			return nil
		}
		_, srv := serve(t, opts)
		body := `{"public_url":"https://` + strings.Repeat("a", admin.MaxPublicURLBody) + `.example.com"}`
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, srv.URL+"/admin/server/public-url", strings.NewReader(body))
		res, err := srv.Client().Do(req)

		if err != nil {
			t.Fatal(err)
		}

		_ = res.Body.Close()

		if res.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("status %d, want 413", res.StatusCode)
		}
	})

	t.Run("other methods are 405", func(t *testing.T) {
		_, srv := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/admin/server/public-url", strings.NewReader(`{}`))
		res, err := srv.Client().Do(req)

		if err != nil {
			t.Fatal(err)
		}

		_ = res.Body.Close()

		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status %d, want 405", res.StatusCode)
		}
	})
}
