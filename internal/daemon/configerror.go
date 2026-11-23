package daemon

import (
	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
)

// configKind translates config's Kind to the admin API's at the seam. The
// switch is exhaustive, so a new config.Kind cannot ship untranslated.
func configKind(k config.Kind) admin.ConfigKind {
	switch k {
	case config.KindConflict:
		return admin.ConfigKindConflict
	case config.KindUnsafe:
		return admin.ConfigKindUnsafe
	case config.KindInvalid:
		return admin.ConfigKindInvalid
	}

	return ""
}

// configCase translates config's Case to the admin API's at the seam. The
// switch is exhaustive, so a new config.Case cannot ship untranslated.
func configCase(c config.Case) admin.ConfigCase {
	switch c {
	case config.CaseConnectorExists:
		return admin.ConfigCaseConnectorExists
	case config.CaseConnectorNotFound:
		return admin.ConfigCaseConnectorNotFound
	case config.CaseEditNotFound:
		return admin.ConfigCaseEditNotFound
	case config.CaseKeyNotSet:
		return admin.ConfigCaseKeyNotSet
	case config.CaseNothingToChange:
		return admin.ConfigCaseNothingToChange
	case config.CaseKeyTwice:
		return admin.ConfigCaseKeyTwice
	case config.CaseUnparsable:
		return admin.ConfigCaseUnparsable
	case config.CaseInlineConnectors:
		return admin.ConfigCaseInlineConnectors
	case config.CaseNotTables:
		return admin.ConfigCaseNotTables
	case config.CaseDefinedElsewhere:
		return admin.ConfigCaseDefinedElsewhere
	case config.CaseReadsDifferently:
		return admin.ConfigCaseReadsDifferently
	case config.CaseDottedKeys:
		return admin.ConfigCaseDottedKeys
	case config.CaseMultiline:
		return admin.ConfigCaseMultiline
	case config.CaseChangesOthers:
		return admin.ConfigCaseChangesOthers
	case config.CaseServerNotTable:
		return admin.ConfigCaseServerNotTable
	}

	return ""
}
