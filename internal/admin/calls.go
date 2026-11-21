package admin

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/atomicpages/wayfindr/internal/store"
)

const (
	// DefaultCallsLimit is GET /admin/calls's limit when the query has none.
	DefaultCallsLimit = 50
	// MaxCallsLimit is the largest limit GET /admin/calls accepts.
	MaxCallsLimit = 1000
)

// CallsQuery selects the calls GET /admin/calls lists.
type CallsQuery struct {
	// Connectors keeps only calls to these connectors; empty keeps all.
	Connectors []string
	// Since keeps only calls that started at or after it; zero keeps all.
	Since time.Time
	// ErrorsOnly keeps only calls whose result is not ok.
	ErrorsOnly bool
	// Limit is the most calls listed, newest first: 1 to MaxCallsLimit,
	// or 0 for DefaultCallsLimit.
	Limit int
}

// Calls is the body of GET /admin/calls.
type Calls struct {
	// Retention is calls.retention as a Go duration, such as "720h0m0s".
	// "0s" means the call log is off: nothing is recorded.
	Retention string `json:"retention"`
	// Calls are the matching calls, newest first. Always an array.
	Calls []Call `json:"calls"`
}

// Call is one row of the call log. It never holds the call's arguments or
// result, only the error text of one that is not ok.
type Call struct {
	// StartedAt is when the call arrived, in UTC.
	StartedAt time.Time `json:"started_at"`
	// Client names the calling client; absent when unknown.
	Client string `json:"client,omitempty"`
	// TokenName names the client token the call authenticated with: "default"
	// or a scoped token's name. Unlike Client, the daemon checked it.
	// Absent when unknown, as for calls logged before tokens were recorded.
	TokenName string `json:"token_name,omitempty"`
	// Connector and Tool are as the agent wrote them; Connector is empty
	// when the name had no dot, and both are when there was no name.
	Connector string `json:"connector"`
	Tool      string `json:"tool"`
	// DurationMS is how long Wayfindr spent on the upstream call,
	// connecting included; absent when the call never reached that point.
	DurationMS *int64           `json:"duration_ms,omitempty"`
	Result     store.CallResult `json:"result"`
	// Kind and Case say why a call that is not ok ended as it did, in the
	// gateway's or the upstream's vocabulary; absent when not known.
	Kind string `json:"kind,omitempty"`
	Case string `json:"case,omitempty"`
	// Error is the error text the agent got back from a call that is not
	// ok, on one line and at most 500 bytes; absent when ok or not known.
	Error string `json:"error,omitempty"`
}

// calls serves GET /admin/calls?connector=&since=&errors=&limit=.
// connector may repeat; since is an RFC 3339 time.
func (a api) calls(w http.ResponseWriter, r *http.Request) {
	q, msg := parseCallsQuery(r.URL.Query())

	if msg != "" {
		http.Error(w, msg, http.StatusBadRequest)

		return
	}

	rows, err := a.Store.ListCalls(r.Context(), store.CallFilter{
		UserID:     a.UserID,
		Connectors: q.Connectors,
		Since:      q.Since,
		ErrorsOnly: q.ErrorsOnly,
		Limit:      q.Limit,
	})

	if err != nil {
		a.Logger.Error().Err(err).Msg("admin calls")
		http.Error(w, "call log unavailable; see the daemon log", http.StatusInternalServerError)

		return
	}

	var retention time.Duration

	if a.CallRetention != nil {
		retention = a.CallRetention()
	}

	res := Calls{Retention: retention.String(), Calls: make([]Call, 0, len(rows))}

	for _, c := range rows {
		call := Call{
			StartedAt: c.StartedAt.UTC(),
			Client:    c.Client,
			TokenName: c.TokenName,
			Connector: c.Connector,
			Tool:      c.Tool,
			Result:    c.Result,
			Kind:      c.ErrorKind,
			Case:      c.ErrorCase,
			Error:     c.ErrorText,
		}

		if c.Duration != nil {
			ms := c.Duration.Milliseconds()
			call.DurationMS = &ms
		}

		res.Calls = append(res.Calls, call)
	}

	writeJSON(w, res)
}

// parseCallsQuery reads GET /admin/calls's query; msg says what is wrong
// with it, or is empty.
func parseCallsQuery(v url.Values) (q CallsQuery, msg string) {
	q.Connectors = v["connector"]
	q.Limit = DefaultCallsLimit

	if s := v.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)

		if err != nil {
			return q, "since must be an RFC 3339 time"
		}

		q.Since = t
	}

	if s := v.Get("errors"); s != "" {
		b, err := strconv.ParseBool(s)

		if err != nil {
			return q, "errors must be true or false"
		}

		q.ErrorsOnly = b
	}

	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)

		if err != nil || n < 1 || n > MaxCallsLimit {
			return q, "limit must be a whole number from 1 to " + strconv.Itoa(MaxCallsLimit)
		}

		q.Limit = n
	}

	return q, ""
}

// Calls calls GET /admin/calls: the calls matching q, newest first, and
// the call log's retention.
func (c *Client) Calls(ctx context.Context, q CallsQuery) (Calls, error) {
	v := url.Values{}

	for _, name := range q.Connectors {
		v.Add("connector", name)
	}

	if !q.Since.IsZero() {
		v.Set("since", q.Since.UTC().Format(time.RFC3339Nano))
	}

	if q.ErrorsOnly {
		v.Set("errors", "true")
	}

	if q.Limit != 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}

	path := pathCalls

	if len(v) > 0 {
		path += "?" + v.Encode()
	}

	var res Calls
	err := c.do(ctx, http.MethodGet, path, nil, &res)

	return res, err
}
