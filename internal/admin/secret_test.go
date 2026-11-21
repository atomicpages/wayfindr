package admin_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// retriedOnTok is what the manager reports after secret tok is set: github
// still needs auth, and linear is ready.
func retriedOnTok() map[string][]upstream.ConnectorStatus {
	return map[string][]upstream.ConnectorStatus{"tok": {
		{Connector: "github", State: upstream.StateNeedsAuth, Reason: "no credential",
			Kind: upstream.KindNeedsAuth, Case: upstream.CaseNoCredential, Link: "http://127.0.0.1:7777/auth/github"},
		{Connector: "linear", State: upstream.StateReady},
	}}
}

func TestPutSecretStoresTheValueAndReportsTheRetriedConnectors(t *testing.T) {
	st, user := newStore(t)
	u := &fakeUpstreams{retried: retriedOnTok()}

	// Each retried connector is reported as its full status row.
	synced := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	if _, err := st.ApplySync(t.Context(), user, store.SyncOutcome{Connector: "github", At: synced,
		Tools: []store.Tool{tool(t, "github", "create_pr")}}); err != nil {
		t.Fatal(err)
	}

	c, _ := serve(t, options(t, st, user, u, admin.ConfigState{}))

	got, err := c.PutSecret(t.Context(), "tok", "s3cret")

	if err != nil {
		t.Fatalf("PutSecret: %v", err)
	}

	want := admin.SecretSet{Name: "tok", Retried: []admin.Connector{
		{Name: "github", State: upstream.StateNeedsAuth, Tools: 1, LastSync: &admin.Sync{At: synced},
			Advice: &admin.Advice{
				Summary: "The connector has not been authorized yet.",
				Action:  "Open the link to authorize it.",
				Link:    "http://127.0.0.1:7777/auth/github",
			}},
		{Name: "linear", State: upstream.StateReady},
	}}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("PutSecret (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff([]string{"tok"}, u.secretsChanged()); diff != "" {
		t.Errorf("SecretChanged calls (-want +got):\n%s", diff)
	}

	sealed, err := st.GetSecret(t.Context(), user, "tok")

	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}

	if string(sealed) != "tok:s3cret" {
		t.Errorf("stored %q, want the value stored under its name, %q", sealed, "tok:s3cret")
	}
}

func TestPutSecretWithNoConnectorWaitingRetriesNone(t *testing.T) {
	st, user := newStore(t)
	c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))

	got, err := c.PutSecret(t.Context(), "tok", "v")

	if err != nil {
		t.Fatalf("PutSecret: %v", err)
	}

	if diff := cmp.Diff(admin.SecretSet{Name: "tok", Retried: []admin.Connector{}}, got); diff != "" {
		t.Errorf("PutSecret (-want +got):\n%s", diff)
	}
}

func TestPutSecretWithoutAWriterIsNotImplemented(t *testing.T) {
	st, user := newStore(t)
	u := &fakeUpstreams{retried: retriedOnTok()}
	opts := options(t, st, user, u, admin.ConfigState{})
	opts.PutSecret = nil
	c, _ := serve(t, opts)

	if _, err := c.PutSecret(t.Context(), "tok", "v"); statusOf(err) != http.StatusNotImplemented {
		t.Errorf("PutSecret = %v, want 501", err)
	}

	if got := u.secretsChanged(); len(got) != 0 {
		t.Errorf("SecretChanged calls = %v, want none: nothing was stored", got)
	}
}

// A failed write is a failed set: no connector is retried.
func TestPutSecretThatFailsToStoreIs500AndRetriesNone(t *testing.T) {
	st, user := newStore(t)
	u := &fakeUpstreams{retried: retriedOnTok()}
	opts := options(t, st, user, u, admin.ConfigState{})
	opts.PutSecret = func(context.Context, string, string) error { return errors.New("store locked") }
	c, _ := serve(t, opts)

	if _, err := c.PutSecret(t.Context(), "tok", "v"); statusOf(err) != http.StatusInternalServerError {
		t.Errorf("PutSecret = %v, want 500", err)
	}

	if got := u.secretsChanged(); len(got) != 0 {
		t.Errorf("SecretChanged calls = %v, want none: nothing was stored", got)
	}
}

// failingReads is a store whose reads for the rows fail; its writes work.
type failingReads struct{ store.Store }

func (failingReads) ToolCounts(context.Context, string) (map[string]int, error) {
	return nil, errors.New("tool counts unavailable")
}

func (failingReads) LastSync(context.Context, string, string) (store.SyncRecord, error) {
	return store.SyncRecord{}, errors.New("last sync unavailable")
}

// The secret is stored by then, so the set succeeded: the rows are
// reported as far as they could be built, and the failure is logged.
func TestPutSecretSucceedsWhenTheRetriedRowsCannotBeRead(t *testing.T) {
	st, user := newStore(t)
	c, _ := serve(t, options(t, failingReads{st}, user, &fakeUpstreams{retried: retriedOnTok()}, admin.ConfigState{}))

	got, err := c.PutSecret(t.Context(), "tok", "s3cret")

	if err != nil {
		t.Fatalf("PutSecret: %v, want success: the secret was stored", err)
	}

	want := admin.SecretSet{Name: "tok", Retried: []admin.Connector{
		{Name: "github", State: upstream.StateNeedsAuth,
			Advice: &admin.Advice{
				Summary: "The connector has not been authorized yet.",
				Action:  "Open the link to authorize it.",
				Link:    "http://127.0.0.1:7777/auth/github",
			}},
		{Name: "linear", State: upstream.StateReady},
	}}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("PutSecret (-want +got):\n%s", diff)
	}

	if sealed, err := st.GetSecret(t.Context(), user, "tok"); err != nil || string(sealed) != "tok:s3cret" {
		t.Errorf("GetSecret = %q, %v; want the value stored", sealed, err)
	}
}

func TestPutSecretRejectsABadNameOrValueAndStoresNothing(t *testing.T) {
	for _, tt := range []struct {
		name, secret, value string
		status              int
		wantErr             string
	}{
		{"name outside the grammar", "-tok", "v", http.StatusBadRequest, "must match"},
		{"name with a slash", "a/b", "v", http.StatusBadRequest, "must match"},
		{"new name with capitals", "GitHubToken", "v", http.StatusBadRequest, `secret name "GitHubToken" must match [a-z0-9][a-z0-9_-]{0,62}`},
		{"new name with a dot", "github.token", "v", http.StatusBadRequest, `secret name "github.token" must match [a-z0-9][a-z0-9_-]{0,62}`},
		{"empty value", "tok", "", http.StatusBadRequest, "secret value is empty"},
		{"value over the limit", "tok", strings.Repeat("x", admin.MaxSecretBody), http.StatusRequestEntityTooLarge, "secret value is too large"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, user := newStore(t)
			c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))

			_, err := c.PutSecret(t.Context(), tt.secret, tt.value)

			if err == nil {
				t.Fatal("PutSecret: want an error")
			}

			if got := statusOf(err); got != tt.status {
				t.Errorf("status = %d, want %d", got, tt.status)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}

			for _, name := range []string{"tok", tt.secret} {
				if _, err := st.GetSecret(t.Context(), user, name); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("GetSecret(%q) after a rejected set: %v, want not found", name, err)
				}
			}
		})
	}
}

// A secret the daemon already knows, stored or referenced by the running
// config, may keep a name of the older, wider rule (wayfindr-vw6h).
func TestPutSecretKeepsAKnownOlderRuleName(t *testing.T) {
	for _, tt := range []struct {
		name   string
		stored bool
		cfg    admin.ConfigState
	}{
		{name: "stored", stored: true},
		{name: "referenced by the config", cfg: admin.ConfigState{SecretUsers: map[string][]string{"Old.Name": {"connectors.github"}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, user := newStore(t)

			if tt.stored {
				if err := st.PutSecret(t.Context(), user, "Old.Name", []byte("before")); err != nil {
					t.Fatal(err)
				}
			}

			c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, tt.cfg))

			if _, err := c.PutSecret(t.Context(), "Old.Name", "v"); err != nil {
				t.Fatalf("PutSecret: %v", err)
			}

			if sealed, err := st.GetSecret(t.Context(), user, "Old.Name"); err != nil || string(sealed) != "Old.Name:v" {
				t.Errorf("GetSecret = %q, %v; want the new value stored", sealed, err)
			}
		})
	}
}

// Whether an older-rule name is stored can't be told when the list fails,
// so the set fails rather than guess.
func TestPutSecretOfAnOlderRuleNameWhenTheListFailsIs500(t *testing.T) {
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
	opts.ListSecrets = func(context.Context) ([]string, error) { return nil, errors.New("store locked") }
	c, _ := serve(t, opts)

	if _, err := c.PutSecret(t.Context(), "Old.Name", "v"); statusOf(err) != http.StatusInternalServerError {
		t.Errorf("PutSecret = %v, want 500", err)
	}

	// A name of the current rule needs no list.
	if _, err := c.PutSecret(t.Context(), "tok", "v"); err != nil {
		t.Errorf("PutSecret(tok) = %v, want success", err)
	}
}

// A body the client never sends: the handler decodes strictly.
func TestPutSecretRejectsABodyThatIsNotExactlyAValue(t *testing.T) {
	for _, tt := range []struct{ name, body string }{
		{"not JSON", `v`},
		{"an unknown field", `{"value":"v","extra":1}`},
		{"trailing data", `{"value":"v"}{"value":"w"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, user := newStore(t)
			_, srv := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/admin/secrets/tok", strings.NewReader(tt.body))

			if err != nil {
				t.Fatal(err)
			}

			resp, err := srv.Client().Do(req)

			if err != nil {
				t.Fatal(err)
			}

			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
			}

			if _, err := st.GetSecret(t.Context(), user, "tok"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetSecret after a rejected set: %v, want not found", err)
			}
		})
	}
}

func TestDeleteSecretRemovesTheStoredValue(t *testing.T) {
	st, user := newStore(t)
	c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))

	if _, err := c.PutSecret(t.Context(), "tok", "s3cret"); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}

	if err := c.DeleteSecret(t.Context(), "tok"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}

	if _, err := st.GetSecret(t.Context(), user, "tok"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetSecret after delete: err = %v, want ErrNotFound", err)
	}

	err := c.DeleteSecret(t.Context(), "tok")

	if statusOf(err) != http.StatusNotFound || err.Error() != `"tok" has no stored secret` {
		t.Errorf("second DeleteSecret = %v, want 404 saying there is no stored secret", err)
	}
}

func TestDeleteSecretWithoutADeleterIsNotImplemented(t *testing.T) {
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
	opts.DeleteSecret = nil
	c, _ := serve(t, opts)

	if err := c.DeleteSecret(t.Context(), "tok"); statusOf(err) != http.StatusNotImplemented {
		t.Errorf("DeleteSecret = %v, want 501", err)
	}
}

func TestDeleteSecretThatFailsIs500WithoutEchoingTheError(t *testing.T) {
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
	opts.DeleteSecret = func(context.Context, string) error { return errors.New("store locked") }
	c, _ := serve(t, opts)

	err := c.DeleteSecret(t.Context(), "tok")

	if statusOf(err) != http.StatusInternalServerError || err.Error() != "deleting the secret failed; see the daemon log" {
		t.Errorf("DeleteSecret = %v, want 500 pointing at the daemon log", err)
	}
}

func TestListSecretsNamesEachStoredSecretAndWhatUsesIt(t *testing.T) {
	st, user := newStore(t)
	cfg := admin.ConfigState{SecretUsers: map[string][]string{
		"embed_key":    {"connectors.fs", "embedding.api_key"},
		"github_token": {"connectors.github"},
		// Referenced, but not stored: not listed.
		"missing": {"connectors.linear"},
	}}
	c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, cfg))

	for _, name := range []string{"github_token", "unused", "embed_key"} {
		if _, err := c.PutSecret(t.Context(), name, "s3cret"); err != nil {
			t.Fatalf("PutSecret(%s): %v", name, err)
		}
	}

	got, err := c.ListSecrets(t.Context())

	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}

	want := admin.SecretList{Secrets: []admin.Secret{
		{Name: "embed_key", UsedBy: []string{"connectors.fs", "embedding.api_key"}},
		{Name: "github_token", UsedBy: []string{"connectors.github"}},
		{Name: "unused", UsedBy: []string{}},
	}}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ListSecrets (-want +got):\n%s", diff)
	}
}

// The JSON is a public contract (`wayfindr secret list --json`): both
// arrays are always arrays, and no value is ever in it.
func TestListSecretsAnswersArraysAndNoValues(t *testing.T) {
	st, user := newStore(t)
	_, srv := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))

	body := func() string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/admin/secrets", nil)

		if err != nil {
			t.Fatal(err)
		}

		resp, err := srv.Client().Do(req)

		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)

		if err != nil {
			t.Fatal(err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /admin/secrets = %d %s", resp.StatusCode, b)
		}

		return strings.TrimSpace(string(b))
	}

	if got, want := body(), `{"secrets":[]}`; got != want {
		t.Errorf("with none stored: body = %s, want %s", got, want)
	}

	if err := st.PutSecret(t.Context(), user, "tok", []byte("sealed-value")); err != nil {
		t.Fatal(err)
	}

	if got, want := body(), `{"secrets":[{"name":"tok","used_by":[]}]}`; got != want {
		t.Errorf("with one stored: body = %s, want %s", got, want)
	}
}

func TestListSecretsWithoutALister(t *testing.T) {
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
	opts.ListSecrets = nil
	c, _ := serve(t, opts)

	if _, err := c.ListSecrets(t.Context()); statusOf(err) != http.StatusNotImplemented {
		t.Errorf("ListSecrets = %v, want 501", err)
	}
}

func TestListSecretsThatFailsIs500WithoutEchoingTheError(t *testing.T) {
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
	opts.ListSecrets = func(context.Context) ([]string, error) { return nil, errors.New("store locked") }
	c, _ := serve(t, opts)

	_, err := c.ListSecrets(t.Context())

	if statusOf(err) != http.StatusInternalServerError || err.Error() != "listing the secrets failed; see the daemon log" {
		t.Errorf("ListSecrets = %v, want 500 pointing at the daemon log", err)
	}
}

func TestAPIErrorTextIsTheDaemonsMessageAlone(t *testing.T) {
	for _, tt := range []struct {
		e    admin.APIError
		want string
	}{
		{admin.APIError{Method: "GET", Path: "/admin/status", Status: 500, Message: "  boom\n"}, "boom"},
		{admin.APIError{Method: "GET", Path: "/admin/status", Status: 502}, "the daemon answered bad gateway"},
	} {
		if got := tt.e.Error(); got != tt.want {
			t.Errorf("Error() = %q, want %q", got, tt.want)
		}
	}
}
