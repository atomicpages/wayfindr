package config

import (
	"maps"
	"slices"
)

// Changes is what a reload must do to move from one config to the next
// (design §7.8). Connector names are sorted, and each connector is in
// exactly one of Added, Removed, URLChanged, Changed, or Unchanged.
type Changes struct {
	// Added connectors are new.
	Added []string
	// Removed connectors are gone. Their catalog entries are dropped; their
	// credentials are kept (ADR 0006).
	Removed []string
	// URLChanged connectors were url connectors before and after, with a
	// different url. Tokens are bound to the old url (RFC 8707), so they go
	// to needs_auth. Other changes to the same connector are implied: it is
	// rebuilt from the new config either way.
	URLChanged []string
	// Changed connectors differ in any other way, including a switch between
	// url and command. They reconnect and re-sync.
	Changed []string
	// Unchanged connectors are identical, defaults included.
	Unchanged []string

	// EmbeddingChanged is true when the [embedding] table was added,
	// removed, or edited.
	EmbeddingChanged bool
	// RestartRequired lists the keys that changed but only take effect on a
	// daemon restart: server.listen, storage.driver, storage.dsn.
	RestartRequired []string
}

// Diff compares two validated configs. A nil before is an empty previous
// config: every connector is Added, and no restart is required.
func Diff(before, after *Config) Changes {
	var ch Changes

	if before == nil {
		ch.Added = sortedKeys(after.Connectors)
		ch.EmbeddingChanged = after.Embedding != nil

		return ch
	}

	for _, name := range sortedKeys(after.Connectors) {
		a := after.Connectors[name]
		b, ok := before.Connectors[name]

		switch {
		case !ok:
			ch.Added = append(ch.Added, name)
		case b.URL != "" && a.URL != "" && b.URL != a.URL:
			ch.URLChanged = append(ch.URLChanged, name)
		case !connectorEqual(b, a):
			ch.Changed = append(ch.Changed, name)
		default:
			ch.Unchanged = append(ch.Unchanged, name)
		}
	}

	for _, name := range sortedKeys(before.Connectors) {
		if _, ok := after.Connectors[name]; !ok {
			ch.Removed = append(ch.Removed, name)
		}
	}

	ch.EmbeddingChanged = !embeddingEqual(before.Embedding, after.Embedding)

	if before.Server.Listen != after.Server.Listen {
		ch.RestartRequired = append(ch.RestartRequired, "server.listen")
	}

	if before.Storage.Driver != after.Storage.Driver {
		ch.RestartRequired = append(ch.RestartRequired, "storage.driver")
	}

	if before.Storage.DSN != after.Storage.DSN {
		ch.RestartRequired = append(ch.RestartRequired, "storage.dsn")
	}

	return ch
}

// connectorEqual compares field by field. maps.Equal and slices.Equal treat
// an empty table or array the same as an absent one. A new field on
// Connector, Auth, or TLS must be added here too, or Diff will miss its
// changes.
func connectorEqual(a, b Connector) bool {
	return a.Name == b.Name &&
		a.URL == b.URL &&
		maps.Equal(a.Headers, b.Headers) &&
		a.Transport == b.Transport &&
		AuthEqual(a.Auth, b.Auth) &&
		TLSEqual(a.TLS, b.TLS) &&
		a.Command == b.Command &&
		slices.Equal(a.Args, b.Args) &&
		maps.Equal(a.Env, b.Env) &&
		a.Timeout == b.Timeout &&
		slices.Equal(a.Tools, b.Tools)
}

// AuthEqual reports whether two auth tables are the same, nil included: the
// comparison Diff makes, and the one auth uses to tell whether a pending
// auth flow still matches its connector (design §7.3).
func AuthEqual(a, b *Auth) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.Type == b.Type &&
		a.ClientID == b.ClientID &&
		a.ClientSecret == b.ClientSecret &&
		slices.Equal(a.Scopes, b.Scopes) &&
		a.AuthorizationServer == b.AuthorizationServer &&
		a.AuthorizationEndpoint == b.AuthorizationEndpoint &&
		a.TokenEndpoint == b.TokenEndpoint &&
		a.ClientMetadataURL == b.ClientMetadataURL &&
		a.RedirectURI == b.RedirectURI &&
		a.PKCE == b.PKCE &&
		a.ValidateIssuer == b.ValidateIssuer &&
		a.ResourceIndicator == b.ResourceIndicator &&
		maps.Equal(a.AuthorizeParams, b.AuthorizeParams) &&
		a.Grant == b.Grant &&
		a.TokenEndpointAuthMethod == b.TokenEndpointAuthMethod &&
		a.DPoP == b.DPoP &&
		a.DeviceAuthorizationEndpoint == b.DeviceAuthorizationEndpoint &&
		ptrEqual(a.SigningKey, b.SigningKey) &&
		ptrEqual(a.Assertion, b.Assertion)
}

// ptrEqual reports whether two optional tables of comparable fields are the
// same, nil included.
func ptrEqual[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}

	return *a == *b
}

// TLSEqual reports whether two tls tables are the same, nil included: the
// comparison Diff makes, and the one a cache of per-connector HTTP clients
// uses to tell whether a client still matches its connector.
func TLSEqual(a, b *TLS) bool {
	if a == nil || b == nil {
		return a == b
	}

	return *a == *b
}

func embeddingEqual(a, b *Embedding) bool {
	if a == nil || b == nil {
		return a == b
	}

	return *a == *b
}
