package daemon_test

import (
	"crypto/sha256"
	"net/http"
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
