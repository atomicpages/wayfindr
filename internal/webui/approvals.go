package webui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// The approvals page (GET /ui/approvals, POST /ui/approvals/{id}/approve and
// /deny): the gated calls of remote clients that wait for the owner (ADR
// 0024). The console answers through the admin API, the only channel that
// can; a remote client cannot reach it.

// approvalRow is one queue entry as the approvals page shows it. ID,
// Client, Tool, Class, and Arguments pass through redact.Visible, as in the
// CLI's listing: a character the owner could not see, or a run of spaces
// that could push text out of view, is shown as an escape.
type approvalRow struct {
	ID     string
	Client string
	Tool   string
	// Class is "read", "write", or "destructive"; empty when unknown.
	Class string
	Badge badge
	// Age is how long ago the call was gated; Expires when the entry lapses.
	Age     string
	Expires string
	// Created is when the entry was created, RFC 3339 with nanoseconds:
	// the answer forms send it back, so an answer applies only to the
	// entry shown, not to another call that got its id after a restart.
	Created string
	// Arguments are all of them, indented, never cut: the owner approves
	// what they read.
	Arguments string
	// Size is the arguments' size as sent, so arguments far larger than
	// what the owner can see stand out.
	Size string
	// CanApprove is set for a pending or denied entry, CanDeny for a
	// pending or granted one (a deny of a granted entry revokes it).
	CanApprove, CanDeny bool
	// Revoke words the deny button of a granted entry.
	Revoke bool
}

// approvalsView is the approvals page's data.
type approvalsView struct{ Rows []approvalRow }

// countWaiting is how many of rows are pending.
func countWaiting(rows []admin.ApprovalRow) int {
	return len(slices.DeleteFunc(slices.Clone(rows), func(r admin.ApprovalRow) bool { return r.State != admin.ApprovalPending }))
}

// approvalState words an entry's state and gives its place in the page's
// order: the ones that wait, then approved, then denied.
func approvalState(state admin.ApprovalState) (badge, int) {
	switch state {
	case admin.ApprovalPending:
		return badge{"warn", "Waiting"}, 0
	case admin.ApprovalGranted:
		return badge{"ok", "Approved"}, 1
	case admin.ApprovalDenied:
		return badge{"err", "Denied"}, 2
	}

	return badge{"info", redact.Visible(string(state))}, 3
}

// until words how long after now t is.
func until(now, t time.Time) string {
	d := t.Sub(now)

	switch {
	case d <= 0:
		return "Expired"
	case d < time.Minute:
		return "In under a minute"
	case d < time.Hour:
		return fmt.Sprintf("In %d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("In %d h", int(d.Hours()))
	}
}

// approvalRows words the queue's entries for the page, waiting ones first
// and each state's oldest first.
func approvalRows(now time.Time, entries []admin.ApprovalRow) []approvalRow {
	entries = slices.Clone(entries)
	slices.SortStableFunc(entries, func(a, b admin.ApprovalRow) int {
		_, ra := approvalState(a.State)
		_, rb := approvalState(b.State)

		if d := ra - rb; d != 0 {
			return d
		}

		return a.Created.Compare(b.Created)
	})

	rows := make([]approvalRow, 0, len(entries))

	for _, e := range entries {
		b, _ := approvalState(e.State)
		rows = append(rows, approvalRow{
			ID:         redact.Visible(e.ID),
			Client:     redact.Visible(e.Client),
			Tool:       redact.Visible(e.Tool),
			Class:      redact.Visible(e.Class),
			Badge:      b,
			Age:        ago(now, e.Created),
			Expires:    until(now, e.Expires),
			Created:    e.Created.UTC().Format(time.RFC3339Nano),
			Arguments:  redact.VisibleArguments(e.Arguments, "  "),
			Size:       admin.FormatBytes(int64(len(e.Arguments))),
			CanApprove: e.State != admin.ApprovalGranted,
			CanDeny:    e.State != admin.ApprovalDenied,
			Revoke:     e.State == admin.ApprovalGranted,
		})
	}

	return rows
}

// approvals serves GET /ui/approvals: the queue's live entries.
func (c *console) approvals(w http.ResponseWriter, r *http.Request, s sess) {
	// One listing gives both the sidebar's count and the table, so they
	// agree and the queue is read once.
	list, err := c.Admin.Approvals(r.Context())

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	v, _, err := c.baseWithApprovals(r, s, "approvals", "Approvals", list.Approvals)

	if err != nil {
		c.failErr(w, r, s, err)

		return
	}

	v.Toast = toast(r)
	v.Body = approvalsView{Rows: approvalRows(c.Now(), list.Approvals)}
	c.render(w, http.StatusOK, "approvals", v)
}

// answerApproval is the handler of one answer: it applies answer to the
// path's id and the form's created time, the entry's as the page showed
// it, and goes back to the list, whose toast says done. An id with no live
// entry, or whose live entry is not the one shown, shows the admin API's
// own refusal. A form without a created time is refused: every form the
// page renders has one.
func (c *console) answerApproval(done string, answer func(context.Context, string, time.Time) error) func(http.ResponseWriter, *http.Request, sess) {
	return func(w http.ResponseWriter, r *http.Request, s sess) {
		id := r.PathValue("id")
		created, err := time.Parse(time.RFC3339Nano, r.PostFormValue("created"))

		if err != nil {
			c.fail(w, r, s, http.StatusBadRequest, "This form is out of date. Reload the approvals page and answer again.")

			return
		}

		if err := answer(r.Context(), id, created); err != nil {
			c.failErr(w, r, s, err)

			return
		}

		http.Redirect(w, r, "/ui/approvals?"+url.Values{"done": {done}, "name": {id}}.Encode(), http.StatusSeeOther)
	}
}
