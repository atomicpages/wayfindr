package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
)

// The OAuth request routes (ADR 0022, design §7.11, admin): arming, reading
// a pending authorization (which claims it), approving, and denying, through
// the admin API's Client against a fake OAuthRequests.

var oauthEpoch = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

// fakeOAuth is OAuthRequests in memory: one claimable request, "r1".
type fakeOAuth struct {
	mu sync.Mutex
	// approveErr and getErr are what Approve and Get return.
	approveErr, getErr error
	armed              int
	got                []string
	approvals          []admin.OAuthApproval
	denied             []string
}

func (f *fakeOAuth) Arm(context.Context) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed++

	return oauthEpoch.Add(10 * time.Minute), nil
}

func (f *fakeOAuth) Get(_ context.Context, id string) (admin.OAuthRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, id)

	if f.getErr != nil {
		return admin.OAuthRequest{}, f.getErr
	}

	if id != "r1" {
		return admin.OAuthRequest{}, fmt.Errorf("pending %q: %w", id, store.ErrNotFound)
	}

	return admin.OAuthRequest{
		ID: "r1", ClientID: "https://chatgpt.com/oauth/client.json", ClientName: "ChatGPT", ClientHost: "chatgpt.com",
		RedirectHost: "chatgpt.com", StartedAt: oauthEpoch, ReportedIP: "203.0.113.7", Name: "chatgpt",
		ExpiresAt: oauthEpoch.Add(10 * time.Minute), Arrived: 2,
	}, nil
}

func (f *fakeOAuth) Approve(_ context.Context, id, name string, connectors []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvals = append(f.approvals, admin.OAuthApproval{Name: name, Connectors: connectors})

	if f.approveErr != nil {
		return "", f.approveErr
	}

	if id != "r1" {
		return "", store.ErrNotFound
	}

	return "https://chatgpt.com/cb?code=C0DE&state=S&iss=I", nil
}

func (f *fakeOAuth) Deny(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denied = append(f.denied, id)

	if id != "r1" {
		return "", store.ErrNotFound
	}

	return "https://chatgpt.com/cb?error=access_denied&state=S&iss=I", nil
}

// serveOAuth serves the admin API over f with github and linear configured
// and OAuth on.
func serveOAuth(t *testing.T, f admin.OAuthRequests, enabled bool) *admin.Client {
	t.Helper()
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{
		Connectors: []string{"github", "linear"},
		PublicURL:  "https://wf.example.com",
		OAuth:      config.OAuth{Enabled: enabled},
	})

	if f != nil {
		opts.OAuth = f
	}

	c, _ := serve(t, opts)

	return c
}

func TestArmOAuth(t *testing.T) {
	t.Run("answers when the arm ends", func(t *testing.T) {
		f := &fakeOAuth{}
		c := serveOAuth(t, f, true)
		got, err := c.ArmOAuth(t.Context())

		if err != nil {
			t.Fatalf("ArmOAuth: %v", err)
		}

		if diff := cmp.Diff(admin.OAuthArm{ExpiresAt: oauthEpoch.Add(10 * time.Minute)}, got); diff != "" {
			t.Errorf("ArmOAuth (-want +got):\n%s", diff)
		}
	})

	t.Run("while OAuth is off is 409 and arms nothing", func(t *testing.T) {
		f := &fakeOAuth{}
		c := serveOAuth(t, f, false)
		_, err := c.ArmOAuth(t.Context())

		if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "Turn on sign-in for cloud agents first.") {
			t.Errorf("ArmOAuth while off = %v, want 409 with the advice", err)
		}

		if f.armed != 0 {
			t.Errorf("the server was armed %d times while OAuth is off", f.armed)
		}
	})

	t.Run("without the daemon's support is 501", func(t *testing.T) {
		if _, err := serveOAuth(t, nil, true).ArmOAuth(t.Context()); statusOf(err) != http.StatusNotImplemented {
			t.Errorf("ArmOAuth with no OAuth option = %v, want 501", err)
		}
	})
}

func TestOAuthRequestClaimsAndAnswersTheRequest(t *testing.T) {
	f := &fakeOAuth{}
	c := serveOAuth(t, f, true)
	got, err := c.OAuthRequest(t.Context(), "r1")

	if err != nil {
		t.Fatalf("OAuthRequest: %v", err)
	}

	want := admin.OAuthRequest{
		ID: "r1", ClientID: "https://chatgpt.com/oauth/client.json", ClientName: "ChatGPT", ClientHost: "chatgpt.com",
		RedirectHost: "chatgpt.com", StartedAt: oauthEpoch, ReportedIP: "203.0.113.7", Name: "chatgpt",
		ExpiresAt: oauthEpoch.Add(10 * time.Minute), Arrived: 2,
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("OAuthRequest (-want +got):\n%s", diff)
	}

	// A reload asks again: nothing in the route keeps state.
	if _, err := c.OAuthRequest(t.Context(), "r1"); err != nil {
		t.Errorf("OAuthRequest again: %v", err)
	}

	if _, err := c.OAuthRequest(t.Context(), "nope"); statusOf(err) != http.StatusNotFound {
		t.Errorf("OAuthRequest of an unknown id = %v, want 404", err)
	}

	if _, err := serveOAuth(t, nil, true).OAuthRequest(t.Context(), "r1"); statusOf(err) != http.StatusNotImplemented {
		t.Errorf("OAuthRequest with no OAuth option = %v, want 501", err)
	}
}

func TestApproveOAuth(t *testing.T) {
	t.Run("answers where the browser goes next", func(t *testing.T) {
		f := &fakeOAuth{}
		c := serveOAuth(t, f, true)
		got, err := c.ApproveOAuth(t.Context(), "r1", admin.OAuthApproval{Name: "chatgpt", Connectors: []string{"linear", "github", "linear"}})

		if err != nil {
			t.Fatalf("ApproveOAuth: %v", err)
		}

		want := admin.OAuthDecision{Redirect: "https://chatgpt.com/cb?code=C0DE&state=S&iss=I", Name: "chatgpt"}

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("ApproveOAuth (-want +got):\n%s", diff)
		}

		// Connectors reach the server without repeats, in the order given.
		if diff := cmp.Diff([]admin.OAuthApproval{{Name: "chatgpt", Connectors: []string{"linear", "github"}}}, f.approvals); diff != "" {
			t.Errorf("what Approve got (-want +got):\n%s", diff)
		}
	})

	refused := []struct {
		name     string
		approval admin.OAuthApproval
		status   int
		text     string
	}{
		{"a bad name", admin.OAuthApproval{Name: "Not A Name", Connectors: []string{"github"}}, http.StatusBadRequest, `connection name "Not A Name" must match`},
		{"the default token's name", admin.OAuthApproval{Name: "default", Connectors: []string{"github"}}, http.StatusBadRequest, "reserved"},
		{"no connector", admin.OAuthApproval{Name: "chatgpt"}, http.StatusBadRequest, "a connection needs at least one connector"},
		{"an unknown connector", admin.OAuthApproval{Name: "chatgpt", Connectors: []string{"nosuch"}}, http.StatusBadRequest, "no connector is called"},
	}

	for _, tt := range refused {
		t.Run(tt.name+" is 400 before the server hears of it", func(t *testing.T) {
			f := &fakeOAuth{}
			_, err := serveOAuth(t, f, true).ApproveOAuth(t.Context(), "r1", tt.approval)

			if statusOf(err) != tt.status || !strings.Contains(err.Error(), tt.text) {
				t.Errorf("ApproveOAuth = %v, want %d containing %q", err, tt.status, tt.text)
			}

			if len(f.approvals) != 0 {
				t.Errorf("Approve was called with %+v", f.approvals)
			}
		})
	}

	fromServer := []struct {
		name   string
		err    error
		status int
		text   string
	}{
		{"a taken name is 409", fmt.Errorf("grant: %w", store.ErrExists), http.StatusConflict, `a client token or OAuth grant called "chatgpt" already exists`},
		{"a contested request is 409 of its own", fmt.Errorf("%w: two arrived", admin.ErrContested), http.StatusConflict, "More than one connection request arrived after you selected Connect a client, so this one can't be approved."},
		{"an unknown request is 404", store.ErrNotFound, http.StatusNotFound, "no such pending authorization"},
		{"a name the server refuses is 400", fmt.Errorf("%w: reserved", admin.ErrInvalidApproval), http.StatusBadRequest, "can't be used as a name"},
		{"anything else is 500 without its text", io.ErrUnexpectedEOF, http.StatusInternalServerError, "see the daemon log"},
	}

	for _, tt := range fromServer {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeOAuth{approveErr: tt.err}
			_, err := serveOAuth(t, f, true).ApproveOAuth(t.Context(), "r1", admin.OAuthApproval{Name: "chatgpt", Connectors: []string{"github"}})

			if statusOf(err) != tt.status || !strings.Contains(err.Error(), tt.text) {
				t.Errorf("ApproveOAuth = %v, want %d containing %q", err, tt.status, tt.text)
			}

			if tt.status == http.StatusInternalServerError && strings.Contains(err.Error(), io.ErrUnexpectedEOF.Error()) {
				t.Errorf("the 500 shows the error's text: %v", err)
			}
		})
	}

	t.Run("an unknown request id is 404", func(t *testing.T) {
		_, err := serveOAuth(t, &fakeOAuth{}, true).ApproveOAuth(t.Context(), "nope", admin.OAuthApproval{Name: "chatgpt", Connectors: []string{"github"}})

		if statusOf(err) != http.StatusNotFound {
			t.Errorf("ApproveOAuth of an unknown id = %v, want 404", err)
		}
	})
}

func TestDenyOAuth(t *testing.T) {
	f := &fakeOAuth{}
	c := serveOAuth(t, f, true)
	got, err := c.DenyOAuth(t.Context(), "r1")

	if err != nil {
		t.Fatalf("DenyOAuth: %v", err)
	}

	if diff := cmp.Diff(admin.OAuthDecision{Redirect: "https://chatgpt.com/cb?error=access_denied&state=S&iss=I"}, got); diff != "" {
		t.Errorf("DenyOAuth (-want +got):\n%s", diff)
	}

	if _, err := c.DenyOAuth(t.Context(), "nope"); statusOf(err) != http.StatusNotFound {
		t.Errorf("DenyOAuth of an unknown id = %v, want 404", err)
	}
}

// TestOAuthRoutesRefuseMalformedAndLargeBodies: bodies are capped like the
// other admin routes, and the arm and the deny take none or {}.
func TestOAuthRoutesRefuseMalformedAndLargeBodies(t *testing.T) {
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{Connectors: []string{"github"}, OAuth: config.OAuth{Enabled: true}})
	opts.OAuth = &fakeOAuth{}
	_, srv := serve(t, opts)

	post := func(path, body string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(body))

		if err != nil {
			t.Fatal(err)
		}

		resp, err := srv.Client().Do(req)

		if err != nil {
			t.Fatal(err)
		}

		_ = resp.Body.Close()

		return resp.StatusCode
	}

	big := `{"name":"chatgpt","connectors":["github"],"pad":"` + strings.Repeat("x", admin.MaxOAuthApprovalBody) + `"}`

	for _, tt := range []struct {
		name, path, body string
		want             int
	}{
		{"approve with an oversized body", "/admin/oauth/requests/r1/approve", big, http.StatusRequestEntityTooLarge},
		{"approve with an unknown field", "/admin/oauth/requests/r1/approve", `{"name":"chatgpt","connectors":["github"],"x":1}`, http.StatusBadRequest},
		{"approve with no body", "/admin/oauth/requests/r1/approve", "", http.StatusBadRequest},
		{"deny with a body", "/admin/oauth/requests/r1/deny", `{"x":1}`, http.StatusBadRequest},
		{"deny with {}", "/admin/oauth/requests/r1/deny", `{}`, http.StatusOK},
		{"arm with {}", "/admin/oauth/arm", `{}`, http.StatusOK},
		{"arm with junk", "/admin/oauth/arm", `[1]`, http.StatusBadRequest},
	} {
		if got := post(tt.path, tt.body); got != tt.want {
			t.Errorf("%s = %d, want %d", tt.name, got, tt.want)
		}
	}
}
