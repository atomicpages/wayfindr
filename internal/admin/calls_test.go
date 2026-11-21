package admin_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/store"
)

func durationMS(n int64) *time.Duration {
	d := time.Duration(n) * time.Millisecond

	return &d
}

// recordFixtureCalls stores three calls a minute apart, from base on,
// oldest first: an ok call, a rejected one, and a failed one.
func recordFixtureCalls(t *testing.T, st store.Store, user string, base time.Time) {
	t.Helper()

	for _, c := range []store.Call{
		{UserID: user, Client: "claude-code", TokenName: "default", Connector: "linear", Tool: "create_issue",
			StartedAt: base, Duration: durationMS(412), Result: store.CallOK},
		{UserID: user, Connector: "github", Tool: "get_file", StartedAt: base.Add(time.Minute),
			Result: store.CallRejected, ErrorKind: "invalid_arguments", ErrorCase: "schema_mismatch"},
		{UserID: user, Client: "cursor", TokenName: "nightly-triage", Connector: "slack", Tool: "post_message", StartedAt: base.Add(2 * time.Minute),
			Duration: durationMS(30_000), Result: store.CallFailed, ErrorKind: "timeout"},
	} {
		if err := st.RecordCall(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
}

func get(t *testing.T, srv interface {
	Client() *http.Client
}, url string,
) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)

	if err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Client().Do(req)

	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, body
}

func TestCallsListsTheCallLogNewestFirst(t *testing.T) {
	st, user := newStore(t)
	// A zone other than UTC: the response normalises every time to UTC.
	base := time.Date(2026, 11, 8, 11, 0, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	recordFixtureCalls(t, st, user, base)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
	opts.CallRetention = func() time.Duration { return 720 * time.Hour }
	c, srv := serve(t, opts)

	// The JSON shape is a public contract (`wayfindr calls --json`), so the
	// wire is pinned byte for byte.
	golden, err := os.ReadFile(filepath.Join("testdata", "calls.json"))

	if err != nil {
		t.Fatal(err)
	}

	code, body := get(t, srv, srv.URL+"/admin/calls")

	if code != http.StatusOK {
		t.Fatalf("GET /admin/calls = %d, want 200; body: %s", code, body)
	}

	var indented bytes.Buffer

	if err := json.Indent(&indented, body, "", "  "); err != nil {
		t.Fatalf("response is not JSON: %v; body: %s", err, body)
	}

	if diff := cmp.Diff(string(golden), indented.String()); diff != "" {
		t.Errorf("response differs from testdata/calls.json (-want +got):\n%s", diff)
	}

	// The client reads the same contract back.
	var want admin.Calls

	if err := json.Unmarshal(golden, &want); err != nil {
		t.Fatal(err)
	}

	got, err := c.Calls(t.Context(), admin.CallsQuery{})

	if err != nil {
		t.Fatalf("Client.Calls: %v", err)
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Client.Calls (-want +got):\n%s", diff)
	}
}

func TestCallsClientSendsEveryFilter(t *testing.T) {
	st, user := newStore(t)
	base := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	recordFixtureCalls(t, st, user, base)
	c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))

	tests := []struct {
		name  string
		query admin.CallsQuery
		want  []string // tools, newest first
	}{
		{"all", admin.CallsQuery{}, []string{"post_message", "get_file", "create_issue"}},
		{"since", admin.CallsQuery{Since: base.Add(time.Minute)}, []string{"post_message", "get_file"}},
		{"errors only", admin.CallsQuery{ErrorsOnly: true}, []string{"post_message", "get_file"}},
		{"connectors", admin.CallsQuery{Connectors: []string{"linear", "slack"}}, []string{"post_message", "create_issue"}},
		{"limit", admin.CallsQuery{Limit: 1}, []string{"post_message"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := c.Calls(t.Context(), tt.query)

			if err != nil {
				t.Fatalf("Client.Calls: %v", err)
			}

			var got []string

			for _, call := range res.Calls {
				got = append(got, call.Tool)
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("tools (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCallsReportsTheLogOffWithAnEmptyArray(t *testing.T) {
	st, user := newStore(t)
	// No CallRetention: the call log is off.
	_, srv := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))
	code, body := get(t, srv, srv.URL+"/admin/calls")

	if want := `{"retention":"0s","calls":[]}`; code != http.StatusOK || strings.TrimSpace(string(body)) != want {
		t.Errorf("GET /admin/calls = %d %s, want 200 %s", code, body, want)
	}
}

func TestCallsRefusesAMalformedQuery(t *testing.T) {
	st, user := newStore(t)
	_, srv := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))

	for _, query := range []string{
		"since=yesterday",
		"errors=maybe",
		"limit=0",
		"limit=1001",
		"limit=ten",
	} {
		t.Run(query, func(t *testing.T) {
			if code, body := get(t, srv, srv.URL+"/admin/calls?"+query); code != http.StatusBadRequest {
				t.Errorf("GET /admin/calls?%s = %d %s, want 400", query, code, body)
			}
		})
	}
}
