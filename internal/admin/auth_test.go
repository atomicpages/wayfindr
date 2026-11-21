package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// fakeFlows stands in for the daemon's adapter of the auth broker. A
// flow's report is in reports by id; an id it doesn't have is unknown, and
// a report with no State waits for the ctx and is pending.
type fakeFlows struct {
	mu      sync.Mutex
	started []string
	err     error
	reports map[string]admin.FlowReport
	// waited is how long the last WaitFlow's ctx allowed.
	waited time.Duration
}

func (f *fakeFlows) StartFlow(_ context.Context, connector string, start admin.AuthStart) (admin.AuthFlow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, connector)

	if f.err != nil {
		return admin.AuthFlow{}, f.err
	}

	if start.Device {
		return admin.AuthFlow{
			FlowID: "f1", Connector: connector, UserCode: "WDJB-MJHT", VerificationURI: "https://as.example/device",
			VerificationURIComplete: "https://as.example/device?user_code=WDJB-MJHT",
			ExpiresAt:               time.Date(2026, 11, 5, 12, 10, 0, 0, time.UTC),
		}, nil
	}

	return admin.AuthFlow{
		FlowID: "f1", Connector: connector, AuthorizeURL: "https://as.example/authorize?client_id=x",
		ExpiresAt: time.Date(2026, 11, 5, 12, 10, 0, 0, time.UTC),
	}, nil
}

func (f *fakeFlows) WaitFlow(ctx context.Context, id string) (admin.FlowReport, bool) {
	f.mu.Lock()
	rep, ok := f.reports[id]

	if d, has := ctx.Deadline(); has {
		f.waited = time.Until(d)
	}

	f.mu.Unlock()

	if !ok {
		return admin.FlowReport{}, false
	}

	if rep.State == "" {
		<-ctx.Done()
		rep.State = admin.FlowPending
	}

	return rep, true
}

func (f *fakeFlows) lastWait() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.waited
}

func serveFlows(t *testing.T, f *fakeFlows) (*admin.Client, store.Store, string) {
	t.Helper()
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
	opts.Flows = f
	c, _ := serve(t, opts)

	return c, st, user
}

func TestStartAuthReturnsTheFlow(t *testing.T) {
	f := &fakeFlows{}
	c, _, _ := serveFlows(t, f)

	got, err := c.StartAuth(t.Context(), "linear", admin.AuthStart{})

	if err != nil {
		t.Fatalf("StartAuth: %v", err)
	}

	want := admin.AuthFlow{
		FlowID: "f1", Connector: "linear", AuthorizeURL: "https://as.example/authorize?client_id=x",
		ExpiresAt: time.Date(2026, 11, 5, 12, 10, 0, 0, time.UTC),
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("StartAuth (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff([]string{"linear"}, f.started); diff != "" {
		t.Errorf("flows started (-want +got):\n%s", diff)
	}
}

// TestStartAuthDeviceFlowJSON pins the wire shape of a device code flow:
// {"device": true} asks for it, and the answer carries the user code and
// the verification URIs.
func TestStartAuthDeviceFlowJSON(t *testing.T) {
	f := &fakeFlows{}
	_, srv := serve(t, func() admin.Options {
		st, user := newStore(t)
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
		opts.Flows = f

		return opts
	}())

	post := func(body string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/admin/auth/linear", strings.NewReader(body))

		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("Content-Type", "application/json")

		return srv.Client().Do(req)
	}

	resp, err := post(`{"device": true}`)

	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()
	var got map[string]any

	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	want := map[string]any{
		"flow_id": "f1", "connector": "linear", "authorize_url": "", "user_code": "WDJB-MJHT",
		"verification_uri":          "https://as.example/device",
		"verification_uri_complete": "https://as.example/device?user_code=WDJB-MJHT",
		"expires_at":                "2026-11-05T12:10:00Z",
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("POST /admin/auth/linear {device} (-want +got):\n%s", diff)
	}

	bad, err := post(`{"device": "yes"}`)

	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	_ = bad.Body.Close()

	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("POST with a malformed body = %d, want 400", bad.StatusCode)
	}

	if n := len(f.started); n != 1 {
		t.Errorf("flows started = %d, want 1: a malformed body starts none", n)
	}
}

func TestStartAuthThatCannotStartSaysWhy(t *testing.T) {
	f := &fakeFlows{err: &admin.FlowError{Status: http.StatusConflict, Advice: admin.Advice{
		Summary: "The authorization server does not advertise PKCE.", Action: "Set client_id.",
	}}}
	c, _, _ := serveFlows(t, f)

	_, err := c.StartAuth(t.Context(), "linear", admin.AuthStart{})

	if statusOf(err) != http.StatusConflict ||
		!strings.Contains(err.Error(), "does not advertise PKCE.\nSet client_id.") {
		t.Errorf("StartAuth = %v, want 409 with the advice", err)
	}
}

func TestWaitAuthReportsADoneFlowWithTheConnectorsRow(t *testing.T) {
	f := &fakeFlows{reports: map[string]admin.FlowReport{"f1": {
		Connector: "linear", State: admin.FlowDone,
		Status: &upstream.ConnectorStatus{Connector: "linear", State: upstream.StateReady,
			Since: time.Date(2026, 11, 5, 12, 1, 0, 0, time.UTC)},
	}}}
	c, st, user := serveFlows(t, f)

	synced := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	if _, err := st.ApplySync(t.Context(), user, store.SyncOutcome{Connector: "linear", At: synced,
		Tools: []store.Tool{tool(t, "linear", "create_issue")}}); err != nil {
		t.Fatal(err)
	}

	got, err := c.WaitAuth(t.Context(), "f1", 30*time.Second)

	if err != nil {
		t.Fatalf("WaitAuth: %v", err)
	}

	want := admin.AuthFlowStatus{FlowID: "f1", Connector: "linear", State: admin.FlowDone, Result: &admin.Connector{
		Name: "linear", State: upstream.StateReady, Since: time.Date(2026, 11, 5, 12, 1, 0, 0, time.UTC), Tools: 1,
		LastSync: &admin.Sync{At: synced},
	}}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("WaitAuth (-want +got):\n%s", diff)
	}
}

func TestWaitAuthReportsAFailedFlowsAdvice(t *testing.T) {
	adv := &admin.Advice{Summary: "The authorization was not granted.", Action: "Try again.", Link: "http://127.0.0.1:7777/auth/linear"}
	f := &fakeFlows{reports: map[string]admin.FlowReport{"f1": {Connector: "linear", State: admin.FlowFailed, Advice: adv}}}
	c, _, _ := serveFlows(t, f)

	got, err := c.WaitAuth(t.Context(), "f1", 0)

	if err != nil {
		t.Fatalf("WaitAuth: %v", err)
	}

	want := admin.AuthFlowStatus{FlowID: "f1", Connector: "linear", State: admin.FlowFailed, Advice: adv}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("WaitAuth (-want +got):\n%s", diff)
	}
}

func TestWaitAuthLongPollsUpToTheWait(t *testing.T) {
	f := &fakeFlows{reports: map[string]admin.FlowReport{
		"f1": {Connector: "linear"},
		"f2": {Connector: "linear", State: admin.FlowExpired},
	}}
	c, _, _ := serveFlows(t, f)

	got, err := c.WaitAuth(t.Context(), "f1", 50*time.Millisecond)

	if err != nil {
		t.Fatalf("WaitAuth: %v", err)
	}

	if got.State != admin.FlowPending {
		t.Errorf("WaitAuth = %+v, want pending once the wait is over", got)
	}

	// f2 has ended, so the call returns at once; its ctx shows the wait.
	if _, err := c.WaitAuth(t.Context(), "f2", time.Hour); err != nil {
		t.Fatalf("WaitAuth: %v", err)
	}

	if w := f.lastWait(); w > admin.MaxFlowWait || w < admin.MaxFlowWait-5*time.Second {
		t.Errorf("a wait of 1h waited %v, want the %v cap", w, admin.MaxFlowWait)
	}
}

func TestWaitAuthErrors(t *testing.T) {
	f := &fakeFlows{reports: map[string]admin.FlowReport{}}
	c, srv := serve(t, func() admin.Options {
		st, user := newStore(t)
		o := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
		o.Flows = f

		return o
	}())

	if _, err := c.WaitAuth(t.Context(), "nope", 0); statusOf(err) != http.StatusNotFound {
		t.Errorf("WaitAuth(unknown) = %v, want 404", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/admin/auth/flows/f1?wait=soon", nil)

	if err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Client().Do(req)

	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("wait=soon = %d, want 400", resp.StatusCode)
	}
}

// A FlowError's text is technical, for logs; the user reads its advice,
// which is the body of the answer (see TestStartAuthThatCannotStartSaysWhy).
func TestFlowErrorTextIsTechnical(t *testing.T) {
	err := &admin.FlowError{Status: http.StatusConflict, Kind: "config", Case: "pkce_unsupported",
		Advice: admin.Advice{Summary: "The authorization server does not advertise PKCE.", Action: "Set client_id."}}

	if got, want := err.Error(), "auth flow: config/pkce_unsupported"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
