package admin

import (
	"errors"
	"net/http"
)

// ConfigKind is the coarse class of a refused change to the daemon's config
// file (GLOSSARY: Kind). It mirrors config's kinds, which the daemon
// translates; the set is closed.
type ConfigKind string

const (
	// ConfigKindConflict means the change clashes with the file, such as a taken
	// connector name, or the file changed while it was being written.
	ConfigKindConflict ConfigKind = "conflict"
	// ConfigKindUnsafe means the change could alter more than the one setting,
	// or the file can't be read well enough to tell.
	ConfigKindUnsafe ConfigKind = "unsafe"
	// ConfigKindInvalid means the value itself is refused, whatever the file
	// holds.
	ConfigKindInvalid ConfigKind = "invalid"
)

// ConfigCase is the finer reason within a [ConfigKind] (GLOSSARY: Case).
// The set is closed. The cases of config's edits are mirrored here, and
// the daemon translates each; the last four are the daemon's own.
type ConfigCase string

const (
	// ConfigCaseConnectorExists (conflict): add, and the name is taken.
	ConfigCaseConnectorExists ConfigCase = "connector_exists"
	// ConfigCaseConnectorNotFound (conflict): remove, and there is no such
	// connector.
	ConfigCaseConnectorNotFound ConfigCase = "connector_not_found"
	// ConfigCaseEditNotFound (conflict): edit, and there is no such
	// connector.
	ConfigCaseEditNotFound ConfigCase = "edit_not_found"
	// ConfigCaseKeyNotSet (conflict): edit, and a key to unset is not set.
	ConfigCaseKeyNotSet ConfigCase = "key_not_set"
	// ConfigCaseNothingToChange (invalid): edit, with no key to change.
	ConfigCaseNothingToChange ConfigCase = "nothing_to_change"
	// ConfigCaseKeyTwice (invalid): edit, and a key is changed twice.
	ConfigCaseKeyTwice ConfigCase = "key_twice"
	// ConfigCaseUnparsable (unsafe): the file does not parse.
	ConfigCaseUnparsable ConfigCase = "unparsable"
	// ConfigCaseInlineConnectors (unsafe): the file's connectors are an
	// inline table.
	ConfigCaseInlineConnectors ConfigCase = "inline_connectors"
	// ConfigCaseNotTables (unsafe): the connector is not written as
	// [connectors.<name>] tables.
	ConfigCaseNotTables ConfigCase = "not_tables"
	// ConfigCaseDefinedElsewhere (unsafe): the connector is also defined
	// outside its tables.
	ConfigCaseDefinedElsewhere ConfigCase = "defined_elsewhere"
	// ConfigCaseReadsDifferently (unsafe): the setting would read
	// differently than written.
	ConfigCaseReadsDifferently ConfigCase = "reads_differently"
	// ConfigCaseDottedKeys (unsafe): a key is written with dotted keys.
	ConfigCaseDottedKeys ConfigCase = "dotted_keys"
	// ConfigCaseMultiline (unsafe): a value spans lines.
	ConfigCaseMultiline ConfigCase = "multiline"
	// ConfigCaseChangesOthers (unsafe): the change would alter other
	// settings.
	ConfigCaseChangesOthers ConfigCase = "changes_others"
	// ConfigCaseServerNotTable (unsafe): server is not written as a [server]
	// table.
	ConfigCaseServerNotTable ConfigCase = "server_not_table"

	// ConfigCaseConfigChanged (conflict): the file changed between the read
	// and the write.
	ConfigCaseConfigChanged ConfigCase = "config_changed"
	// ConfigCaseInvalidConnector (invalid): the config's validation refused
	// the new connector.
	ConfigCaseInvalidConnector ConfigCase = "invalid_connector"
	// ConfigCaseInvalidSpec (invalid): the connector spec itself is wrong,
	// before it reaches the config.
	ConfigCaseInvalidSpec ConfigCase = "invalid_spec"
	// ConfigCaseInvalidPublicURL (invalid): server.public_url is not a
	// valid URL.
	ConfigCaseInvalidPublicURL ConfigCase = "invalid_public_url"
)

// ConfigError is a change to the daemon's config file that it refused, such
// as a taken or invalid connector name or an invalid public URL: the Kind and
// Case, and the advice, whose text is the response body. The HTTP status
// follows from the Kind alone (see status). It is what Options.AddConnector
// and Options.SetPublicURL return for a refusal.
type ConfigError struct {
	Kind   ConfigKind
	Case   ConfigCase
	Advice Advice
}

// status is the HTTP status that answers e, chosen from its Kind in this one
// place: an invalid value is the caller's to fix (400); a conflict, or an
// unsafe edit of the file, is a clash with what the file holds (409).
func (e *ConfigError) status() int {
	switch e.Kind {
	case ConfigKindInvalid:
		return http.StatusBadRequest
	case ConfigKindConflict, ConfigKindUnsafe:
		return http.StatusConflict
	}

	return http.StatusConflict
}

// Error is technical text for logs: the Kind and Case. What the error
// means to the user is its Advice.
func (e *ConfigError) Error() string {
	return "change config: " + string(e.Kind) + "/" + string(e.Case)
}

// writeConfigError answers err as its *ConfigError, with the advice as the
// body, and reports whether it was one; the caller handles any other error.
func writeConfigError(w http.ResponseWriter, err error) bool {
	ce := (*ConfigError)(nil)

	if !errors.As(err, &ce) {
		return false
	}

	http.Error(w, ce.Advice.text(), ce.status())

	return true
}
