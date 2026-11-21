package config

import "errors"

// Kind is the coarse class of a refused edit of the config file
// (GLOSSARY: Kind). The set is closed.
type Kind string

const (
	// KindConflict means the edit clashes with the file: the connector's
	// name is already taken, there is no such connector to edit or remove,
	// or a key to unset is not there.
	KindConflict Kind = "conflict"
	// KindUnsafe means the edit could change more than the one connector,
	// or the file can't be read well enough to tell.
	KindUnsafe Kind = "unsafe"
	// KindInvalid means the edit itself makes no sense, whatever the file
	// holds: it changes nothing, or names a key twice.
	KindInvalid Kind = "invalid"
)

// Case is the finer reason within a [Kind] (GLOSSARY: Case). The set is
// closed.
type Case string

const (
	// CaseConnectorExists (conflict): add, and the file has a connector of
	// that name.
	CaseConnectorExists Case = "connector_exists"
	// CaseConnectorNotFound (conflict): remove, and the file has no
	// connector of that name.
	CaseConnectorNotFound Case = "connector_not_found"
	// CaseEditNotFound (conflict): edit, and the file has no connector of
	// that name. It is not CaseConnectorNotFound because its advice points
	// at add instead of the list of connectors.
	CaseEditNotFound Case = "edit_not_found"
	// CaseKeyNotSet (conflict): edit, and a key to unset is not in the
	// connector's table.
	CaseKeyNotSet Case = "key_not_set"
	// CaseNothingToChange (invalid): edit, with no key to set or unset.
	CaseNothingToChange Case = "nothing_to_change"
	// CaseKeyTwice (invalid): edit, and a key is set or unset twice, or
	// inside another key the edit sets or unsets.
	CaseKeyTwice Case = "key_twice"
	// CaseUnparsable (unsafe): the file does not parse, before or after the
	// edit.
	CaseUnparsable Case = "unparsable"
	// CaseInlineConnectors (unsafe): add, and the file's connectors are an
	// inline table, which a [connectors.<name>] header can't extend.
	CaseInlineConnectors Case = "inline_connectors"
	// CaseNotTables (unsafe): remove or edit, and the connector is not
	// written as [connectors.<name>] tables the edit can find (a dotted key
	// or an inline table under [connectors]).
	CaseNotTables Case = "not_tables"
	// CaseDefinedElsewhere (unsafe): remove, and the connector is still
	// there without its tables.
	CaseDefinedElsewhere Case = "defined_elsewhere"
	// CaseReadsDifferently (unsafe): add or edit, and the connector would
	// read differently than the edit wrote it.
	CaseReadsDifferently Case = "reads_differently"
	// CaseDottedKeys (unsafe): edit, and a key it changes is written with
	// dotted keys (auth.type = …) inside the connector's table.
	CaseDottedKeys Case = "dotted_keys"
	// CaseMultiline (unsafe): edit, and the value of a key it changes
	// spans lines.
	CaseMultiline Case = "multiline"
	// CaseChangesOthers (unsafe): the edit would change settings other than
	// the one connector.
	CaseChangesOthers Case = "changes_others"
)

// Error is a refused edit of the config file; the file is left as it was.
// AppendConnector, RemoveConnector and EditConnector return it as error.
type Error struct {
	Kind      Kind
	Case      Case
	Connector string
	// Key is the key of the connector's table the Case is about, as TOML
	// writes it (auth.scopes), for CaseKeyNotSet, CaseKeyTwice,
	// CaseDottedKeys and CaseMultiline.
	Key string
	// Err is the parse error, for CaseUnparsable.
	Err error
}

// Error is technical, for logs; [Advise] has the user's wording.
func (e *Error) Error() string {
	s := "config edit " + string(e.Case) + ": connector " + e.Connector

	if e.Key != "" {
		s += ": key " + e.Key
	}

	if e.Err != nil {
		s += ": " + e.Err.Error()
	}

	return s
}

// Unwrap returns the cause, if any.
func (e *Error) Unwrap() error { return e.Err }

// AsError narrows err to the config [*Error] it carries, if any.
func AsError(err error) (*Error, bool) {
	var e *Error

	if errors.As(err, &e) {
		return e, true
	}

	return nil, false
}

// Advice is config's user-facing reading of an [*Error] (GLOSSARY: Advice):
// what went wrong and the action that fixes it. The CLI is its only
// presenter, so it is worded as the CLI's errors are: lowercase clauses.
type Advice struct {
	Summary string
	Action  string
}

// Advise says what e means to the user and how to fix it. Its switch on
// [Case] is exhaustive (the exhaustive linter enforces it), so a new Case
// cannot ship without advice.
func Advise(e *Error) Advice {
	if e == nil {
		return Advice{}
	}

	const (
		unsafe = "could not change it safely: "
		byHand = "edit the file by hand"
	)

	switch e.Case {
	case CaseConnectorExists:
		return Advice{
			e.Connector + " already exists",
			"change it with `wayfindr connector edit " + e.Connector + "`, or remove it with `wayfindr connector remove " + e.Connector + "`",
		}
	case CaseConnectorNotFound:
		return Advice{"no connector is called " + e.Connector, "`wayfindr status` lists them"}
	case CaseEditNotFound:
		return Advice{"no connector is called " + e.Connector, "`wayfindr connector add` creates one"}
	case CaseKeyNotSet:
		return Advice{e.Connector + " has no " + e.Key, "leave it out, or check its spelling"}
	case CaseNothingToChange:
		return Advice{"nothing to change", "give the flags to set or --unset"}
	case CaseKeyTwice:
		return Advice{e.Key + " is changed twice in one edit", "set or unset each key once"}
	case CaseUnparsable:
		return Advice{unsafe + "it does not parse, or would not after the edit", byHand}
	case CaseInlineConnectors:
		return Advice{unsafe + "its connectors are an inline table", byHand}
	case CaseNotTables:
		return Advice{unsafe + "connector " + e.Connector + " is not written as a [connectors." + e.Connector + "] table", byHand}
	case CaseDefinedElsewhere:
		return Advice{unsafe + "connector " + e.Connector + " is also defined outside its tables", byHand}
	case CaseReadsDifferently:
		return Advice{unsafe + "connector " + e.Connector + " would read differently than written", byHand}
	case CaseDottedKeys:
		return Advice{unsafe + "connector " + e.Connector + " writes " + e.Key + " with dotted keys", byHand}
	case CaseMultiline:
		return Advice{unsafe + "the value of " + e.Connector + "'s " + e.Key + " spans lines", byHand}
	case CaseChangesOthers:
		return Advice{unsafe + "the edit would change other settings", byHand}
	}

	return Advice{}
}
