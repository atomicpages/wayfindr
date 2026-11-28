package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// fakeApprovals stands in for the daemon's adapter of the gateway's queue:
// it lists rows and records each answer, with the created time it named,
// if any. An id it doesn't hold is admin.ErrNoApproval, and a created time
// other than the row's admin.ErrApprovalChanged, wrapped as the daemon
// wraps them.
type fakeApprovals struct {
	mu      sync.Mutex
	rows    []admin.ApprovalRow
	answers []string
	err     error
}

func (f *fakeApprovals) List(context.Context) ([]admin.ApprovalRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.rows), f.err
}

func (f *fakeApprovals) Approve(_ context.Context, id string, created time.Time) error {
	return f.answer("approve", id, created)
}

func (f *fakeApprovals) Deny(_ context.Context, id string, created time.Time) error {
	return f.answer("deny", id, created)
}

func (f *fakeApprovals) answer(verb, id string, created time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}

	i := slices.IndexFunc(f.rows, func(r admin.ApprovalRow) bool { return r.ID == id })

	if i < 0 {
		return fmt.Errorf("%w: %s", admin.ErrNoApproval, id)
	}

	if !created.IsZero() && !created.Equal(f.rows[i].Created) {
		return fmt.Errorf("%w: %s", admin.ErrApprovalChanged, id)
	}

	answer := verb + " " + id

	if !created.IsZero() {
		answer += " created " + created.UTC().Format(time.RFC3339Nano)
	}

	f.answers = append(f.answers, answer)

	return nil
}

func (f *fakeApprovals) answered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.answers)
}

// serveApprovals serves the admin API over f.
func serveApprovals(t *testing.T, f admin.Approvals) (*admin.Client, *httptest.Server) {
	t.Helper()
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{Connectors: []string{"slack"}})
	opts.Approvals = f

	return serve(t, opts)
}

// pendingPost is a pending approval of a slack post, created in UTC+2.
func pendingPost() admin.ApprovalRow {
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.FixedZone("UTC+2", 2*60*60))

	return admin.ApprovalRow{
		ID: "a3f2", Client: "grok-bot", Tool: "slack.post_message", Class: "write",
		Arguments: json.RawMessage(`{"channel":"C01","text":"Deploy done"}`),
		State:     "pending", Created: at, Expires: at.Add(24 * time.Hour),
	}
}

func TestListApprovals(t *testing.T) {
	c, srv := serveApprovals(t, &fakeApprovals{rows: []admin.ApprovalRow{pendingPost()}})

	code, body := get(t, srv, srv.URL+"/admin/approvals")

	if code != http.StatusOK {
		t.Fatalf("GET /admin/approvals = %d, want 200; body: %s", code, body)
	}

	// The arguments are the raw JSON, and every time is UTC.
	want := `{"approvals":[{"id":"a3f2","client":"grok-bot","tool":"slack.post_message","class":"write",` +
		`"arguments":{"channel":"C01","text":"Deploy done"},"state":"pending",` +
		`"created":"2026-11-27T10:00:00Z","expires":"2026-11-28T10:00:00Z"}]}` + "\n"

	if diff := cmp.Diff(want, string(body)); diff != "" {
		t.Errorf("body (-want +got):\n%s", diff)
	}

	got, err := c.Approvals(t.Context())

	if err != nil {
		t.Fatalf("Approvals: %v", err)
	}

	created := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	wantRow := pendingPost()
	wantRow.Created, wantRow.Expires = created, created.Add(24*time.Hour)

	if diff := cmp.Diff(admin.ApprovalList{Approvals: []admin.ApprovalRow{wantRow}}, got); diff != "" {
		t.Errorf("Approvals (-want +got):\n%s", diff)
	}
}

func TestListApprovalsWithNoneIsAnEmptyArray(t *testing.T) {
	_, srv := serveApprovals(t, &fakeApprovals{})

	if code, body := get(t, srv, srv.URL+"/admin/approvals"); code != http.StatusOK || string(body) != `{"approvals":[]}`+"\n" {
		t.Errorf("GET /admin/approvals = %d %s, want 200 {\"approvals\":[]}", code, body)
	}
}

func TestListApprovalsFailureIs500(t *testing.T) {
	c, _ := serveApprovals(t, &fakeApprovals{err: errors.New("boom")})

	if _, err := c.Approvals(t.Context()); statusOf(err) != http.StatusInternalServerError {
		t.Errorf("Approvals = %v, want 500", err)
	}
}

func TestApproveAndDenyApproval(t *testing.T) {
	f := &fakeApprovals{rows: []admin.ApprovalRow{pendingPost()}}
	c, srv := serveApprovals(t, f)

	if err := c.ApproveApproval(t.Context(), "a3f2", time.Time{}); err != nil {
		t.Fatalf("ApproveApproval: %v", err)
	}

	if err := c.DenyApproval(t.Context(), "a3f2", time.Time{}); err != nil {
		t.Fatalf("DenyApproval: %v", err)
	}

	if diff := cmp.Diff([]string{"approve a3f2", "deny a3f2"}, f.answered()); diff != "" {
		t.Errorf("answers (-want +got):\n%s", diff)
	}

	// The wire answer is 204 with no body, and the request's body is
	// optional: none, or {} name no created time.
	for _, verb := range []string{"approve", "deny"} {
		for _, in := range []string{"", "{}", " \n"} {
			if code, body := post(t, srv, srv.URL+"/admin/approvals/a3f2/"+verb, in); code != http.StatusNoContent || len(body) != 0 {
				t.Errorf("POST %s with body %q = %d %q, want 204 with no body", verb, in, code, body)
			}
		}
	}
}

// An answer that names the created time of the entry the owner was shown
// applies only to that entry: the id of a live entry created at another
// time (the queue starts empty after a restart, so an id from an older
// listing can name a new call) is refused with 409, changing nothing.
func TestAnswerApprovalIsBoundToTheEntryShown(t *testing.T) {
	row := pendingPost()
	row.Created = row.Created.Add(123456789 * time.Nanosecond)
	f := &fakeApprovals{rows: []admin.ApprovalRow{row}}
	c, srv := serveApprovals(t, f)
	answers := map[string]func(context.Context, string, time.Time) error{"approve": c.ApproveApproval, "deny": c.DenyApproval}

	t.Run("another created time is 409", func(t *testing.T) {
		for verb, answer := range answers {
			err := answer(t.Context(), "a3f2", row.Created.Add(-time.Hour))

			if want := `approval "a3f2" is not the call you were shown; list approvals again`; statusOf(err) != http.StatusConflict || err.Error() != want {
				t.Errorf("%s = %v, want 409 %q", verb, err, want)
			}
		}

		if got := f.answered(); len(got) != 0 {
			t.Errorf("answers = %v, want none", got)
		}
	})

	t.Run("the shown created time answers, to the nanosecond, in any zone", func(t *testing.T) {
		for _, verb := range []string{"approve", "deny"} {
			if err := answers[verb](t.Context(), "a3f2", row.Created); err != nil {
				t.Errorf("%s: %v", verb, err)
			}
		}

		body := `{"created":"` + row.Created.In(time.FixedZone("UTC-5", -5*60*60)).Format(time.RFC3339Nano) + `"}`

		if code, got := post(t, srv, srv.URL+"/admin/approvals/a3f2/approve", body); code != http.StatusNoContent {
			t.Errorf("POST approve %s = %d %q, want 204", body, code, got)
		}

		want := "created 2026-11-27T10:00:00.123456789Z"

		if diff := cmp.Diff([]string{"approve a3f2 " + want, "deny a3f2 " + want, "approve a3f2 " + want}, f.answered()); diff != "" {
			t.Errorf("answers (-want +got):\n%s", diff)
		}
	})

	t.Run("a body that isn't an answer is 400", func(t *testing.T) {
		before := len(f.answered())

		for _, in := range []string{
			`{"created":"yesterday"}`,
			`{"created":"0001-01-01T00:00:00Z"}`,
			`{"created":"2026-11-27T10:00:00Z","id":"b7c1"}`,
			`{"created":"2026-11-27T10:00:00Z"} {}`,
			`[]`,
			`{"created":"` + strings.Repeat("9", 2<<10) + `"}`,
		} {
			if code, _ := post(t, srv, srv.URL+"/admin/approvals/a3f2/approve", in); code != http.StatusBadRequest {
				t.Errorf("POST approve %.40s = %d, want 400", in, code)
			}
		}

		if got := f.answered(); len(got) != before {
			t.Errorf("answers = %v, want no more", got)
		}
	})
}

func TestAnswerUnknownApprovalIs404(t *testing.T) {
	f := &fakeApprovals{rows: []admin.ApprovalRow{pendingPost()}}
	c, _ := serveApprovals(t, f)

	for name, answer := range map[string]func(context.Context, string, time.Time) error{
		"approve": c.ApproveApproval,
		"deny":    c.DenyApproval,
	} {
		t.Run(name, func(t *testing.T) {
			err := answer(t.Context(), "b7c1", time.Time{})

			if statusOf(err) != http.StatusNotFound || !strings.Contains(err.Error(), `no approval "b7c1" is waiting`) {
				t.Errorf("%s = %v, want 404 saying no such approval is waiting", name, err)
			}
		})
	}

	if got := f.answered(); len(got) != 0 {
		t.Errorf("answers = %v, want none", got)
	}
}

func TestAnswerApprovalFailureIs500(t *testing.T) {
	c, _ := serveApprovals(t, &fakeApprovals{err: errors.New("boom")})

	if err := c.ApproveApproval(t.Context(), "a3f2", time.Time{}); statusOf(err) != http.StatusInternalServerError {
		t.Errorf("ApproveApproval = %v, want 500", err)
	}
}

func TestApprovalsWithoutQueueAreNotImplemented(t *testing.T) {
	st, user := newStore(t)
	c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{Connectors: []string{"slack"}}))

	_, errList := c.Approvals(t.Context())
	errApprove := c.ApproveApproval(t.Context(), "a3f2", time.Time{})
	errDeny := c.DenyApproval(t.Context(), "a3f2", time.Time{})

	for name, err := range map[string]error{"list": errList, "approve": errApprove, "deny": errDeny} {
		if statusOf(err) != http.StatusNotImplemented {
			t.Errorf("%s = %v, want 501", name, err)
		}
	}
}
