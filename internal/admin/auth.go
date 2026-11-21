package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/atomicpages/wayfindr/internal/upstream"
)

// MaxFlowWait caps the ?wait= of GET /admin/auth/flows/{id}.
const MaxFlowWait = 60 * time.Second

// FlowState is where an auth flow stands, as the admin API reports it.
type FlowState string

// Flow states.
const (
	// FlowPending: the browser has not come back yet, or the connector's
	// reconnect after it did has not settled.
	FlowPending FlowState = "pending"
	// FlowDone: the token is stored and the connector has reconnected;
	// AuthFlowStatus.Result is its row.
	FlowDone FlowState = "done"
	// FlowFailed: the flow failed; AuthFlowStatus.Advice says why.
	FlowFailed FlowState = "failed"
	// FlowExpired: the browser did not come back in time.
	FlowExpired FlowState = "expired"
)

// maxAuthStartBody bounds the body of POST /admin/auth/{connector}.
const maxAuthStartBody = 1 << 10

// AuthStart is the optional JSON body of POST /admin/auth/{connector}. No
// body is the zero value: the flow the connector's grant names.
type AuthStart struct {
	// Device asks for the device code flow whatever the connector's grant.
	Device bool `json:"device,omitempty"`
}

// AuthFlow is the response to POST /admin/auth/{connector}. An
// authorization code flow has AuthorizeURL, the URL to open in a browser.
// A device code flow has instead UserCode, the code to enter at
// VerificationURI, and, if the server sent one, VerificationURIComplete,
// which has the code in it; both URIs are https or loopback http.
type AuthFlow struct {
	FlowID                  string    `json:"flow_id"`
	Connector               string    `json:"connector"`
	AuthorizeURL            string    `json:"authorize_url"`
	UserCode                string    `json:"user_code,omitempty"`
	VerificationURI         string    `json:"verification_uri,omitempty"`
	VerificationURIComplete string    `json:"verification_uri_complete,omitempty"`
	ExpiresAt               time.Time `json:"expires_at"`
}

// Device reports whether f is a device code flow.
func (f AuthFlow) Device() bool { return f.UserCode != "" }

// AuthFlowStatus is the response to GET /admin/auth/flows/{id}.
type AuthFlowStatus struct {
	FlowID    string    `json:"flow_id"`
	Connector string    `json:"connector"`
	State     FlowState `json:"state"`
	// Advice says why a failed or expired flow ended and how to retry.
	Advice *Advice `json:"advice,omitempty"`
	// Result is the connector's row once the flow is done: its state after
	// the reconnect the new token started, and its tools.
	Result *Connector `json:"result,omitempty"`
}

// FlowReport is how a flow stands, as Flows reports it.
type FlowReport struct {
	Connector string
	State     FlowState
	// Advice is set for a failed or expired flow.
	Advice *Advice
	// Status is the connector's status after its reconnect, for a done flow.
	Status *upstream.ConnectorStatus
}

// Flows starts and waits on auth flows. The daemon adapts auth's flow
// entry (auth.Flows), which the browser's pages use too, to it; admin
// cannot import auth (design §2).
type Flows interface {
	// StartFlow starts an auth flow of connector: the device code when
	// start asks for it, else the one its grant names. An error that is a
	// *FlowError is answered with its status and its advice as text; any
	// other is a 500.
	StartFlow(ctx context.Context, connector string, start AuthStart) (AuthFlow, error)
	// WaitFlow waits until the flow called id has ended, or ctx ends, and
	// reports it; a flow still running then is pending. It reports false
	// for an id it doesn't know.
	WaitFlow(ctx context.Context, id string) (FlowReport, bool)
}

// FlowError is a flow that cannot start: auth's Kind and Case, the HTTP
// status auth gives it (whose problem it is), and auth's advice for the
// user. The daemon copies them from auth's error and words nothing.
type FlowError struct {
	Status     int
	Kind, Case string
	Advice     Advice
}

// Error is technical text for logs: the Kind and Case. What the error
// means to the user is its Advice.
func (e *FlowError) Error() string { return "auth flow: " + e.Kind + "/" + e.Case }

// text is the advice as the body of an error response: one line each for
// the summary, the action, and the link, those that are set.
func (a Advice) text() string {
	var lines []string

	for _, s := range []string{a.Summary, a.Action, a.Link} {
		if s != "" {
			lines = append(lines, s)
		}
	}

	return strings.Join(lines, "\n")
}

// startAuth serves POST /admin/auth/{connector}.
func (a api) startAuth(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("connector")

	if a.Flows == nil {
		http.Error(w, "auth flows are not available", http.StatusNotImplemented)

		return
	}

	var start AuthStart

	if r.ContentLength != 0 {
		if err := readJSON(w, r, maxAuthStartBody, &start); err != nil {
			http.Error(w, `the body must be {"device": true|false}`, http.StatusBadRequest)

			return
		}
	}

	f, err := a.Flows.StartFlow(r.Context(), name, start)

	if fe := (*FlowError)(nil); errors.As(err, &fe) {
		http.Error(w, fe.Advice.text(), fe.Status)

		return
	}

	if err != nil {
		a.Logger.Error().Err(err).Str("connector", name).Msg("start auth flow")
		http.Error(w, "starting the auth flow failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	writeJSON(w, f)
}

// waitAuth serves GET /admin/auth/flows/{id}?wait=<duration>: it waits up
// to wait (none by default, at most MaxFlowWait) for the flow to end.
func (a api) waitAuth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if a.Flows == nil {
		http.Error(w, "auth flows are not available", http.StatusNotImplemented)

		return
	}

	var wait time.Duration

	if s := r.URL.Query().Get("wait"); s != "" {
		d, err := time.ParseDuration(s)

		if err != nil || d < 0 {
			http.Error(w, fmt.Sprintf("wait %.32q is not a duration such as 30s", s), http.StatusBadRequest)

			return
		}

		wait = min(d, MaxFlowWait)
	}

	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	rep, ok := a.Flows.WaitFlow(ctx, id)

	if !ok {
		http.Error(w, "no such auth flow; it may have ended long ago", http.StatusNotFound)

		return
	}

	out := AuthFlowStatus{FlowID: id, Connector: rep.Connector, State: rep.State, Advice: rep.Advice}

	if rep.Status != nil {
		// The context of the request, not the wait: the rows are read after
		// the wait has ended.
		rows := a.rows(r.Context(), []upstream.ConnectorStatus{*rep.Status})
		out.Result = &rows[0]
	}

	writeJSON(w, out)
}
