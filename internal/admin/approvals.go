package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Approvals lists and answers the gateway's pending approvals (ADR 0024).
// The daemon adapts the gateway's queue to it; admin cannot import gateway
// (design §2). The admin API is the only channel that answers one: a remote
// client cannot reach /admin/.
type Approvals interface {
	// List returns the queue's live entries: pending, granted but not yet
	// spent, and denied, none of them expired.
	List(ctx context.Context) ([]ApprovalRow, error)
	// Approve grants the entry id. created is the Created of the entry the
	// owner was shown, or zero for whatever entry id names. An id with no
	// live entry is an error wrapping ErrNoApproval, and one whose live
	// entry was created at another time an error wrapping
	// ErrApprovalChanged.
	Approve(ctx context.Context, id string, created time.Time) error
	// Deny denies the entry id, or revokes it if it was granted. created
	// and the errors are as for Approve.
	Deny(ctx context.Context, id string, created time.Time) error
}

// ErrNoApproval means no live approval has the id: it never existed, it
// expired, or it was spent.
var ErrNoApproval = errors.New("no such approval")

// ErrApprovalChanged means the live approval with the id is not the one
// the owner was shown: it was created at another time. The queue starts
// empty after a restart, so an id from an older listing can name a new
// call.
var ErrApprovalChanged = errors.New("approval is not the call shown")

// ApprovalAnswer is the optional JSON body of POST
// /admin/approvals/{id}/approve and /deny.
type ApprovalAnswer struct {
	// Created is the created time of the entry the owner was shown, as the
	// listing gave it. When set, the answer is refused with 409 unless the
	// live entry id was created at that instant.
	Created *time.Time `json:"created,omitempty"`
}

// maxApprovalAnswerBody bounds an ApprovalAnswer: one RFC 3339 time.
const maxApprovalAnswerBody = 1 << 10

// ApprovalList is the response to GET /admin/approvals.
type ApprovalList struct {
	// Approvals are the queue's live entries. Always an array.
	Approvals []ApprovalRow `json:"approvals"`
}

// ApprovalState is where an entry of the approval queue stands (GLOSSARY:
// Pending approval). The daemon converts the gateway's states to these.
type ApprovalState string

// Approval states.
const (
	// ApprovalPending: the call waits for the owner's answer.
	ApprovalPending ApprovalState = "pending"
	// ApprovalGranted: the owner approved it; the client's next matching
	// call runs, once.
	ApprovalGranted ApprovalState = "granted"
	// ApprovalDenied: the owner denied it; matching calls are refused until
	// it expires.
	ApprovalDenied ApprovalState = "denied"
)

// ApprovalRow is one entry of the approval queue.
type ApprovalRow struct {
	ID string `json:"id"`
	// Client is the name of the client token or OAuth grant that made the
	// call.
	Client string `json:"client"`
	// Tool is the qualified name, <connector>.<tool>.
	Tool string `json:"tool"`
	// Class is "read", "write", or "destructive" (GLOSSARY: Tool class).
	Class string `json:"class"`
	// Arguments are the call's arguments, exactly as the approval binds
	// them. They may hold anything the agent sent; the admin API never logs
	// them.
	Arguments json.RawMessage `json:"arguments"`
	State     ApprovalState   `json:"state"`
	Created   time.Time       `json:"created"`
	Expires   time.Time       `json:"expires"`
}

// withApprovals serves h, a /admin/approvals route, only when the daemon
// passed Options.Approvals; otherwise it answers 501.
func (a api) withApprovals(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Approvals == nil {
			http.Error(w, "approvals are not available", http.StatusNotImplemented)

			return
		}

		h(w, r)
	}
}

// listApprovals serves GET /admin/approvals.
func (a api) listApprovals(w http.ResponseWriter, r *http.Request) {
	rows, err := a.Approvals.List(r.Context())

	if err != nil {
		a.Logger.Error().Err(err).Msg("list approvals")
		http.Error(w, "listing approvals failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	out := make([]ApprovalRow, 0, len(rows))

	for _, row := range rows {
		row.Created, row.Expires = row.Created.UTC(), row.Expires.UTC()
		out = append(out, row)
	}

	writeJSON(w, ApprovalList{Approvals: out})
}

// approveApproval serves POST /admin/approvals/{id}/approve. It answers 204.
func (a api) approveApproval(w http.ResponseWriter, r *http.Request) {
	a.answerApproval(w, r, "approve", a.Approvals.Approve)
}

// denyApproval serves POST /admin/approvals/{id}/deny. It answers 204.
func (a api) denyApproval(w http.ResponseWriter, r *http.Request) {
	a.answerApproval(w, r, "deny", a.Approvals.Deny)
}

// answerApproval applies answer, Approvals' Approve or Deny, to the path's
// id and the body's created time, if any. verb names it in the log and the
// error.
func (a api) answerApproval(w http.ResponseWriter, r *http.Request, verb string, answer func(context.Context, string, time.Time) error) {
	id := r.PathValue("id")
	created, err := readApprovalAnswer(w, r)

	if err != nil {
		http.Error(w, `the body must be empty or {"created": "<RFC 3339 time>"}`, http.StatusBadRequest)

		return
	}

	err = answer(r.Context(), id, created)

	// %.64q bounds the echoed id.
	switch {
	case errors.Is(err, ErrNoApproval):
		http.Error(w, fmt.Sprintf("no approval %.64q is waiting: it expired, was spent, or never existed", id), http.StatusNotFound)

		return
	case errors.Is(err, ErrApprovalChanged):
		http.Error(w, fmt.Sprintf("approval %.64q is not the call you were shown; list approvals again", id), http.StatusConflict)

		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Str("approval_id", id).Str("answer", verb).Msg("answer approval")
		http.Error(w, verb+" failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	a.Logger.Info().Str("approval_id", id).Str("answer", verb).Msg("approval answered")
	w.WriteHeader(http.StatusNoContent)
}

// readApprovalAnswer reads an answer's body: none, or an ApprovalAnswer
// with no other field and nothing after it. The created time is zero when
// the body names none; a body that names the zero time is refused, since
// zero means "whatever entry the id names".
func readApprovalAnswer(w http.ResponseWriter, r *http.Request) (time.Time, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxApprovalAnswerBody))

	if err != nil {
		return time.Time{}, err
	}

	if len(bytes.TrimSpace(body)) == 0 {
		return time.Time{}, nil
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	var ans ApprovalAnswer

	if err := dec.Decode(&ans); err != nil {
		return time.Time{}, errBody
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return time.Time{}, errBody
	}

	if ans.Created == nil {
		return time.Time{}, nil
	}

	if ans.Created.IsZero() {
		return time.Time{}, errBody
	}

	return *ans.Created, nil
}

// Approvals calls GET /admin/approvals: the approval queue's live entries.
func (c *Client) Approvals(ctx context.Context) (ApprovalList, error) {
	var res ApprovalList
	err := c.do(ctx, http.MethodGet, pathApprovals, nil, &res)

	return res, err
}

// ApproveApproval calls POST /admin/approvals/{id}/approve. created is the
// Created of the entry the owner was shown, or zero to send no body and
// approve whatever entry id names. An id with no live entry is an
// *APIError with status 404, and one whose live entry was created at
// another time an *APIError with status 409.
func (c *Client) ApproveApproval(ctx context.Context, id string, created time.Time) error {
	return c.answerApproval(ctx, id, "approve", created)
}

// DenyApproval calls POST /admin/approvals/{id}/deny. created and the
// errors are as for ApproveApproval.
func (c *Client) DenyApproval(ctx context.Context, id string, created time.Time) error {
	return c.answerApproval(ctx, id, "deny", created)
}

// answerApproval posts verb for id, with created in the body unless it is
// zero.
func (c *Client) answerApproval(ctx context.Context, id, verb string, created time.Time) error {
	var in any

	if !created.IsZero() {
		in = ApprovalAnswer{Created: &created}
	}

	return c.do(ctx, http.MethodPost, pathApprovals+"/"+url.PathEscape(id)+"/"+verb, in, nil)
}
