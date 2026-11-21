// Package admin owns the daemon's admin API (design §7.7): its routes,
// their handlers, the request and response types, and the Client the CLI
// calls them with. httpd mounts New's handler at /admin/ behind its
// loopback Host, Origin, and bearer guards; admin checks none of them.
//
// An endpoint is one route in New, one handler, and one Client method, so
// the path and the JSON live in this package only. The response types are
// a public contract: `wayfindr status --json` prints them as they arrive.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// Upstreams reports connector states and reconnects the connectors a
// stored secret changes. The daemon adapts its manager and its recovery
// module to it, so the reconnected connectors' syncs are done by the time
// SecretChanged returns.
type Upstreams interface {
	Status() []upstream.ConnectorStatus
	// SecretChanged reconnects the connectors waiting on secret name and
	// returns their statuses, sorted by name, once each has settled.
	SecretChanged(ctx context.Context, name string) []upstream.ConnectorStatus
}

// Store is the part of store.Store the admin API reads and writes.
type Store interface {
	ToolCounts(ctx context.Context, userID string) (map[string]int, error)
	LastSync(ctx context.Context, userID, connector string) (store.SyncRecord, error)
	ListCredentialConnectors(ctx context.Context, userID string) ([]string, error)
	ListCalls(ctx context.Context, f store.CallFilter) ([]store.Call, error)
}

// ConfigState is what the daemon's current config contributes to Status.
type ConfigState struct {
	// Connectors names every connector in the running config.
	Connectors []string
	// Profiles are the running config's profiles by name (ADR 0011).
	Profiles map[string]*config.Scope
	// ConfigError is why the last reload failed; empty when it did not.
	ConfigError string
	// SecretUsers maps each secret the running config refers to to what
	// refers to it, sorted (config.Config.SecretUsers).
	SecretUsers map[string][]string
}

// Options is what New needs. The daemon builds it once.
type Options struct {
	Upstreams Upstreams
	Store     Store
	UserID    string
	// PutSecret seals and stores the user's secret name. The daemon passes
	// its resolver's, which opens what it stores. Nil answers a secret set
	// with 501.
	PutSecret func(ctx context.Context, name, value string) error
	// DeleteSecret removes the user's secret name, and returns an error
	// wrapping store.ErrNotFound when there is none. Nil answers a secret
	// delete with 501.
	DeleteSecret func(ctx context.Context, name string) error
	// ListSecrets returns the sorted names of the user's stored secrets,
	// never their values. Nil answers a secret list with 501.
	ListSecrets func(ctx context.Context) ([]string, error)
	// Version is the daemon's build version, reported in Status.
	Version string
	// Config returns the daemon's current config state. It is called once
	// per request that needs it: status, token create, and secret list and
	// set.
	Config func() ConfigState
	// Flows starts and waits on auth flows.
	Flows Flows
	// Credentials revokes credentials. Nil answers a revoke with 501.
	Credentials Credentials
	// Health reads token expiries and warnings. Nil leaves
	// token_expires_at and warnings out of every row.
	Health Health
	// CallRetention returns the call log's current calls.retention. Nil
	// reports the call log as off.
	CallRetention func() time.Duration
	// Search runs a search as search_tools would. A refused request is a
	// *SearchError. Nil answers a search with 501. The daemon adapts the
	// gateway to it; admin cannot import gateway (design §2).
	Search func(ctx context.Context, req SearchRequest) (SearchResult, error)
	// Tools lists a connector's tools live from its upstream, classed and
	// checked against its tools list. A listing it can't make is a
	// *ToolsError. Nil answers with 501.
	Tools func(ctx context.Context, connector string) (ToolList, error)
	// Tokens creates, lists, and revokes scoped client tokens. Nil answers
	// every /admin/tokens request with 501.
	Tokens ClientTokens
	// UILogin issues a one-time web console login (ADR 0016). The daemon
	// adapts the console's sessions to it; admin cannot import webui
	// (design §2). Nil answers POST /admin/ui/login with 501.
	UILogin func() UILogin
	// AddConnector adds a connector to the daemon's config file and reports
	// it once it has connected (ADR 0017). A refusal is an *AddError. Nil
	// answers POST /admin/connectors with 501.
	AddConnector func(ctx context.Context, spec ConnectorSpec) (ConnectorReport, error)
	Logger       zerolog.Logger
}

// The routes' paths. Client builds its requests from the same constants.
const (
	pathStatus     = "/admin/status"
	pathSecrets    = "/admin/secrets"
	pathAuth       = "/admin/auth/"
	pathFlows      = "/admin/auth/flows/"
	pathCreds      = "/admin/credentials/" //nolint:gosec // G101: a route, not a credential
	pathCalls      = "/admin/calls"
	pathSearch     = "/admin/search"
	pathTools      = "/admin/tools/"
	pathTokens     = "/admin/tokens"
	pathUILogin    = "/admin/ui/login"
	pathConnectors = "/admin/connectors"
)

// New returns the handler for every /admin/* route. A path it doesn't know
// gets 404, and a known path with the wrong method 405.
func New(opts Options) http.Handler {
	a := api{opts}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pathStatus, a.status)
	mux.HandleFunc("GET "+pathSecrets, a.listSecrets)
	mux.HandleFunc("PUT "+pathSecrets+"/{name}", a.putSecret)
	mux.HandleFunc("DELETE "+pathSecrets+"/{name}", a.deleteSecret)
	mux.HandleFunc("POST "+pathAuth+"{connector}", a.startAuth)
	mux.HandleFunc("GET "+pathFlows+"{id}", a.waitAuth)
	mux.HandleFunc("DELETE "+pathCreds+"{connector}", a.revoke)
	mux.HandleFunc("GET "+pathCalls, a.calls)
	mux.HandleFunc("POST "+pathSearch, a.search)
	mux.HandleFunc("GET "+pathTools+"{connector}", a.tools)
	mux.HandleFunc("POST "+pathTokens, a.withTokens(a.createToken))
	mux.HandleFunc("GET "+pathTokens, a.withTokens(a.listTokens))
	mux.HandleFunc("DELETE "+pathTokens+"/{name}", a.withTokens(a.revokeToken))
	mux.HandleFunc("POST "+pathUILogin, a.uiLogin)
	mux.HandleFunc("POST "+pathConnectors, a.addConnector)

	return mux
}

// api holds the handlers' shared dependencies.
type api struct{ Options }

// errBody means a request body is not exactly one JSON value of the
// expected shape.
var errBody = errors.New("malformed body")

// readJSON decodes r's body, at most limit bytes, into v. It refuses
// unknown fields and anything after the value. The error wraps
// *http.MaxBytesError for a body over limit and errBody otherwise; it
// never quotes the body, which may hold a secret.
func readJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)

	if err == nil {
		if err = dec.Decode(&struct{}{}); errors.Is(err, io.EOF) {
			return nil
		}
	}

	if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
		return mbe
	}

	return errBody
}

// writeJSON answers 200 with v as JSON.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// The status line is sent; a failed write has nobody to tell.
	_ = json.NewEncoder(w).Encode(v)
}
