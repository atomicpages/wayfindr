package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// MaxSecretBody bounds the body of PUT /admin/secrets/{name}: a secret is
// a token or a key, never a file.
const MaxSecretBody = 64 << 10

// SecretValue is the body of PUT /admin/secrets/{name}.
type SecretValue struct {
	Value string `json:"value"`
}

// SecretBodySize is the size of the body Client.PutSecret sends for value,
// which the daemon holds to MaxSecretBody. It is more than len(value): the
// {"value":"…"} wrapper adds 12 bytes, and JSON escaping grows a quote or
// a backslash to two bytes and a control character, `<`, `>`, or `&` to
// six. The CLI compares it with MaxSecretBody to refuse a value the daemon
// would answer 413 before sending it.
func SecretBodySize(value string) int {
	// A SecretValue always marshals.
	b, _ := json.Marshal(SecretValue{Value: value})

	return len(b)
}

// SecretSet is the response to PUT /admin/secrets/{name}.
type SecretSet struct {
	// Name is the secret's name.
	Name string `json:"name"`
	// Retried are the connectors that were waiting on the secret
	// (upstream.Manager.SecretChanged), sorted by name, each in the state
	// its reconnect left it in. Always an array.
	Retried []Connector `json:"retried"`
}

// SecretList is the response to GET /admin/secrets.
type SecretList struct {
	// Secrets are the stored secrets, sorted by name. Always an array.
	Secrets []Secret `json:"secrets"`
}

// Secret is one stored secret as GET /admin/secrets lists it, never its
// value.
type Secret struct {
	Name string `json:"name"`
	// UsedBy is what in the daemon's running config refers to the secret,
	// sorted: "connectors.<name>" or "embedding.api_key". Empty when
	// nothing does. Always an array.
	UsedBy []string `json:"used_by"`
}

// listSecrets serves GET /admin/secrets: the stored secrets' names, each
// with what the running config refers to it from. Only the names are read;
// no value is opened.
func (a api) listSecrets(w http.ResponseWriter, r *http.Request) {
	if a.ListSecrets == nil {
		http.Error(w, "listing secrets is not available", http.StatusNotImplemented)

		return
	}

	names, err := a.ListSecrets(r.Context())

	if err != nil {
		a.Logger.Error().Err(err).Msg("list secrets")
		http.Error(w, "listing the secrets failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	users := a.Config().SecretUsers
	res := SecretList{Secrets: []Secret{}}

	for _, name := range names {
		res.Secrets = append(res.Secrets, Secret{Name: name, UsedBy: append([]string{}, users[name]...)})
	}

	writeJSON(w, res)
}

// putSecret serves PUT /admin/secrets/{name}. It stores the value through
// Options.PutSecret, then has the manager reconnect every connector waiting
// on the secret, and reports how each came out. Once the value is stored
// the set has succeeded: it answers 200 even when a row's store reads fail.
// Connectors that already resolved an older value pick the new one up on
// their next connect (design §4). A new secret's name follows the
// connector-name rule; a known one may keep an older-rule name
// (knownSecret). It never logs the value or the request body (design §9).
func (a api) putSecret(w http.ResponseWriter, r *http.Request) {
	if a.PutSecret == nil {
		http.Error(w, "storing secrets is not available", http.StatusNotImplemented)

		return
	}

	name := r.PathValue("name")
	known, err := a.knownSecret(r.Context(), name)

	if err != nil {
		a.Logger.Error().Err(err).Msg("list secrets")
		http.Error(w, "storing the secret failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	if err := config.CheckSecretName(name, known); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	var body SecretValue

	if err := readJSON(w, r, MaxSecretBody, &body); err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			http.Error(w, "secret value is too large", http.StatusRequestEntityTooLarge)

			return
		}

		http.Error(w, `body must be {"value": "…"}`, http.StatusBadRequest)

		return
	}

	if body.Value == "" {
		http.Error(w, "secret value is empty", http.StatusBadRequest)

		return
	}

	ctx := r.Context()

	if err := a.PutSecret(ctx, name, body.Value); err != nil {
		a.Logger.Error().Err(err).Str("secret", name).Msg("store secret")
		http.Error(w, "storing the secret failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	a.Logger.Info().Str("secret", name).Msg("secret set")
	writeJSON(w, SecretSet{Name: name, Retried: a.rows(ctx, a.Upstreams.SecretChanged(ctx, name))})
}

// knownSecret reports whether name is a secret the daemon already knows:
// the running config refers to it, or it is stored. Only such a secret may
// keep a name of the older, wider rule (config.CheckSecretName). It looks
// only for a name that needs it, so a set under the current rule reads
// nothing.
func (a api) knownSecret(ctx context.Context, name string) (bool, error) {
	if config.IsSecretName(name) || !config.IsSecretRefName(name) {
		return false, nil
	}

	if _, ok := a.Config().SecretUsers[name]; ok {
		return true, nil
	}

	if a.ListSecrets == nil {
		return false, nil
	}

	names, err := a.ListSecrets(ctx)

	if err != nil {
		return false, err
	}

	return slices.Contains(names, name), nil
}

// deleteSecret serves DELETE /admin/secrets/{name}: it removes the secret
// through Options.DeleteSecret and answers 204. A name with no stored
// secret is 404. It reconnects nothing: a connector that resolved the
// value keeps it until its next connect.
func (a api) deleteSecret(w http.ResponseWriter, r *http.Request) {
	if a.DeleteSecret == nil {
		http.Error(w, "deleting secrets is not available", http.StatusNotImplemented)

		return
	}

	name := r.PathValue("name")

	if err := a.DeleteSecret(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// %.64q bounds the echoed name.
			http.Error(w, fmt.Sprintf("%.64q has no stored secret", name), http.StatusNotFound)

			return
		}

		a.Logger.Error().Err(err).Str("secret", name).Msg("delete secret")
		http.Error(w, "deleting the secret failed; see the daemon log", http.StatusInternalServerError)

		return
	}

	a.Logger.Info().Str("secret", name).Msg("secret deleted")
	w.WriteHeader(http.StatusNoContent)
}

// rows are the rows of statuses, in order, built as far as the store
// allows: the secret is stored by the time they are read, so a failed read
// is logged and its field left empty, never turned into a failed set.
func (a api) rows(ctx context.Context, statuses []upstream.ConnectorStatus) []Connector {
	rows := []Connector{}

	if len(statuses) == 0 {
		return rows
	}

	counts, err := a.Store.ToolCounts(ctx, a.UserID)

	if err != nil {
		a.Logger.Error().Err(err).Msg("read tool counts of retried connectors")
	}

	for _, cs := range statuses {
		c, err := a.connector(ctx, cs, counts)

		if err != nil {
			a.Logger.Error().Err(err).Str("connector", cs.Connector).Msg("read last sync of retried connector")
		}

		rows = append(rows, c)
	}

	return rows
}
