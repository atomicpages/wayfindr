package daemon_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/drivers"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// Scoped client tokens (docs/internal/plans/scoped-client-tokens.md): the
// daemon accepts a token from its creation, refuses it from its revoke,
// loads the stored ones at startup, and records their last use once a
// minute and at shutdown.

func (r running) admin() *admin.Client { return admin.NewClient("http://"+r.addr, "", r.client) }

// statusAs asks for /admin/status with token as the bearer and returns the
// status code: 403 for a scoped token the daemon accepts, 401 for one it
// doesn't know.
func (r running) statusAs(t *testing.T, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+r.addr+"/admin/status", nil)

	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	// No kept connection: one would hold up the daemon's shutdown.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)

	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}

func tokenConfig(t *testing.T) map[string]any {
	t.Helper()

	return map[string]any{
		"server":     map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"files": stdioConnector(t, fakeupstream.V20251125)},
	}
}

func TestScopedTokenIsAcceptedFromCreationAndRefusedFromItsRevoke(t *testing.T) {
	h := newHost(t)
	clock := newFakeClock()
	opts := h.options(t)
	opts.Clock = clock
	r := serve(t, h, opts, tokenConfig(t))
	clock.waitArmed(t, epoch.Add(time.Minute))

	created, err := r.admin().CreateToken(t.Context(), admin.TokenSpec{Name: "nightly", Connectors: []string{"files"}})

	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if !created.CreatedAt.Equal(epoch) {
		t.Errorf("CreatedAt = %v, want the daemon clock's %v", created.CreatedAt, epoch)
	}

	if code := r.statusAs(t, created.Token); code != http.StatusForbidden {
		t.Fatalf("/admin/status with the new scoped token = %d, want 403 (accepted, but not for admin)", code)
	}

	list, err := r.admin().ListTokens(t.Context())

	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}

	if len(list.Tokens) != 1 || list.Tokens[0].LastUsedAt != nil {
		t.Fatalf("before the minute is up: %+v, want one token not yet marked used", list.Tokens)
	}

	// The minute passes: the use is recorded, and the next flush armed.
	clock.advance(time.Minute)
	clock.waitArmed(t, epoch.Add(2*time.Minute))
	list, err = r.admin().ListTokens(t.Context())

	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}

	// Used at the daemon clock's epoch, before it moved.
	if len(list.Tokens) != 1 || list.Tokens[0].LastUsedAt == nil || !list.Tokens[0].LastUsedAt.Equal(epoch) {
		t.Fatalf("after a minute: %+v, want the token last used at %v", list.Tokens, epoch)
	}

	if _, err := r.admin().RevokeToken(t.Context(), "nightly"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	if code := r.statusAs(t, created.Token); code != http.StatusUnauthorized {
		t.Errorf("/admin/status with the revoked token = %d, want 401", code)
	}
}

func TestScopedTokensSurviveARestartWithTheirLastUse(t *testing.T) {
	h := newHost(t)
	r := serve(t, h, h.options(t), tokenConfig(t))

	created, err := r.admin().CreateToken(t.Context(), admin.TokenSpec{Name: "nightly", Connectors: []string{"files"}})

	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if code := r.statusAs(t, created.Token); code != http.StatusForbidden {
		t.Fatalf("/admin/status with the new scoped token = %d, want 403", code)
	}

	// Well within the minute: only the flush at shutdown records the use.
	r.stop()

	st, err := drivers.Open(t.Context(), "sqlite", h.paths.DBFile())

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatal(err)
	}

	rows, err := st.ListClientTokens(t.Context(), user)
	_ = st.Close()

	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 || rows[0].LastUsedAt == nil {
		t.Fatalf("stored tokens after shutdown = %+v, want nightly with its last use", rows)
	}

	want := store.ClientToken{UserID: user, Name: "nightly", Connectors: []string{"files"}, CreatedAt: created.CreatedAt}
	got := rows[0]
	got.Digest, got.LastUsedAt = [sha256.Size]byte{}, nil

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("stored token (-want +got):\n%s", diff)
	}

	// The restarted daemon loads it before it serves.
	r = start(t, h, h.options(t))

	if code := r.statusAs(t, created.Token); code != http.StatusForbidden {
		t.Errorf("/admin/status with the token after a restart = %d, want 403", code)
	}
}

// Approval gates through the running daemon (ADR 0024): the admin API's
// approval routes reach the gateway's queue and refuse a scoped token, and
// a remote token's approval entries reach the store. They sit here, not in
// a file of their own, because one that sorts before auth_test.go leaves
// its stdio child stopping when that file's goleak checks run.

// codeAs sends method path with token as the bearer and returns the status
// code.
func (r running) codeAs(t *testing.T, method, path, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+r.addr+path, nil)

	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	// No kept connection: one would hold up the daemon's shutdown.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)

	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}

// TestApprovalRoutesAreDefaultTokenOnly: only the default token answers an
// approval. A remote token, the kind whose calls are gated, gets the admin
// API's 403 on every approval route, so it can't approve its own call.
func TestApprovalRoutesAreDefaultTokenOnly(t *testing.T) {
	h := newHost(t)
	r := serve(t, h, h.options(t), tokenConfig(t))

	created, err := r.admin().CreateToken(t.Context(), admin.TokenSpec{
		Name: "grok", Connectors: []string{"files"}, Remote: true, Approval: []string{"@write"},
	})

	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/admin/approvals"},
		{http.MethodPost, "/admin/approvals/a3f2/approve"},
		{http.MethodPost, "/admin/approvals/a3f2/deny"},
	} {
		if code := r.codeAs(t, route.method, route.path, created.Token); code != http.StatusForbidden {
			t.Errorf("%s %s with the remote token = %d, want 403", route.method, route.path, code)
		}
	}

	// The default token reaches the gateway's queue: empty, and an id it
	// doesn't hold is 404.
	list, err := r.admin().Approvals(t.Context())

	if err != nil {
		t.Fatalf("Approvals: %v", err)
	}

	if diff := cmp.Diff(admin.ApprovalList{Approvals: []admin.ApprovalRow{}}, list); diff != "" {
		t.Errorf("Approvals (-want +got):\n%s", diff)
	}

	for name, err := range map[string]error{
		"approve": r.admin().ApproveApproval(t.Context(), "a3f2", time.Time{}),
		"deny":    r.admin().DenyApproval(t.Context(), "a3f2", time.Time{}),
	} {
		if ae := (*admin.APIError)(nil); !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
			t.Errorf("%s of an unknown id = %v, want 404", name, err)
		}
	}
}

// TestTokenApprovalReachesTheStore: a remote token's approval entries are
// answered, listed, and stored as given.
func TestTokenApprovalReachesTheStore(t *testing.T) {
	h := newHost(t)
	r := serve(t, h, h.options(t), tokenConfig(t))
	entries := []string{"@write", "files.delete_*", "!files.delete_draft"}

	created, err := r.admin().CreateToken(t.Context(), admin.TokenSpec{
		Name: "grok", Connectors: []string{"files"}, Remote: true, Approval: entries,
	})

	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if diff := cmp.Diff(entries, created.Approval); diff != "" {
		t.Errorf("created approval (-want +got):\n%s", diff)
	}

	list, err := r.admin().ListTokens(t.Context())

	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}

	if len(list.Tokens) != 1 || !cmp.Equal(entries, list.Tokens[0].Approval) {
		t.Errorf("ListTokens = %+v, want grok with approval %v", list.Tokens, entries)
	}

	r.stop()

	st, err := drivers.Open(t.Context(), "sqlite", h.paths.DBFile())

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	defer func() { _ = st.Close() }()

	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatal(err)
	}

	rows, err := st.ListClientTokens(t.Context(), user)

	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 || !rows[0].Remote || !cmp.Equal(entries, rows[0].Approval) {
		t.Errorf("stored tokens = %+v, want grok, remote, with approval %v", rows, entries)
	}
}

// TestAdminCallCarriesThePendingApprovalsID: a gated call run by hand with a
// remote token (`wayfindr call --token-file`) comes back with the id of the
// approval it waits on beside its refusal, the id the queue lists. After the
// owner approves it, the call runs and carries none. The approval names
// the listed entry's created time, which the gateway checks through the
// admin API, to the nanosecond: another time is 409.
func TestAdminCallCarriesThePendingApprovalsID(t *testing.T) {
	h := newHost(t)
	r := serve(t, h, h.options(t), tokenConfig(t))
	r.waitStatus(t, "files synced", synced("files"))
	c := r.admin()

	created, err := c.CreateToken(t.Context(), admin.TokenSpec{
		Name: "grok", Connectors: []string{"files"}, Remote: true, Approval: []string{"files.echo"},
	})

	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	req := admin.CallRequest{Name: "files.echo", Arguments: json.RawMessage(`{"text":"hi"}`), Client: admin.CallClientCLI, Token: created.Token}
	res, err := c.Call(t.Context(), req)

	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	list, err := c.Approvals(t.Context())

	if err != nil {
		t.Fatalf("Approvals: %v", err)
	}

	if len(list.Approvals) != 1 || res.Approval == "" || res.Approval != list.Approvals[0].ID {
		t.Fatalf("call approval %q, queue %+v, want the one pending entry's id", res.Approval, list.Approvals)
	}

	if got, err := res.Decode(); err != nil || !got.IsError || !strings.Contains(got.Content[0].Text, "(approval "+res.Approval+")") {
		t.Errorf("result %+v (%v), want the refusal naming the approval", got, err)
	}

	shown := list.Approvals[0]

	for name, answer := range map[string]func(context.Context, string, time.Time) error{"approve": c.ApproveApproval, "deny": c.DenyApproval} {
		if ae := (*admin.APIError)(nil); !errors.As(answer(t.Context(), shown.ID, shown.Created.Add(time.Nanosecond)), &ae) || ae.Status != http.StatusConflict {
			t.Errorf("%s with another created time = %v, want 409", name, ae)
		}
	}

	if err := c.ApproveApproval(t.Context(), shown.ID, shown.Created); err != nil {
		t.Fatalf("ApproveApproval: %v", err)
	}

	res, err = c.Call(t.Context(), req)

	if err != nil {
		t.Fatalf("Call after approval: %v", err)
	}

	if got, err := res.Decode(); err != nil || got.IsError || res.Approval != "" {
		t.Errorf("after approval: result %+v (%v), approval %q, want the tool's result and no approval", got, err, res.Approval)
	}
}
