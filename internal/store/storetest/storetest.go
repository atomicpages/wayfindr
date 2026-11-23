// Package storetest is the shared behavior suite for store.Store. Every driver
// runs it from its own tests, so the drivers agree on what the interface
// promises, search included wherever it has one right answer (design §7.5).
// Drivers lists them for tests that compare drivers. Driver-specific behavior
// (connection settings, index internals) stays in the driver's own tests.
package storetest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/atomicpages/wayfindr/internal/store"
)

// Database is one empty database that a driver prepared for a single test.
type Database struct {
	// Open opens a store on the database, applying migrations. Every call
	// opens the same database again.
	Open func(ctx context.Context) (store.Store, error)
	// SQL opens a raw handle on the database, so a test can arrange a state
	// the Store interface cannot reach, such as a newer schema version. The
	// caller closes it.
	SQL func(t *testing.T) *sql.DB
}

// NewDatabase returns a fresh, empty database for one test. It may skip t
// when the driver's server is unavailable.
type NewDatabase func(t *testing.T) Database

// Run runs the suite, one subtest per behavior, each on its own database.
func Run(t *testing.T, newDatabase NewDatabase) {
	t.Helper()
	tests := []struct {
		name string
		run  func(t *testing.T, newDatabase NewDatabase)
	}{
		// Migrations and the local user.
		{"EnsureUserCreatesOneUUIDv7UserAndReturnsItAfterReopen", testEnsureUserCreatesOneUUIDv7UserAndReturnsItAfterReopen},
		{"OpenRefusesDatabaseFromNewerBinary", testOpenRefusesDatabaseFromNewerBinary},
		// Catalog.
		{"ApplySyncIntoEmptyCatalogReportsEveryToolChanged", testApplySyncIntoEmptyCatalogReportsEveryToolChanged},
		{"ApplySyncReturnsOnlyChangedHashesAndDeletesMissingRows", testApplySyncReturnsOnlyChangedHashesAndDeletesMissingRows},
		{"ApplySyncStoresNewDefinitionWhenHashIsUnchanged", testApplySyncStoresNewDefinitionWhenHashIsUnchanged},
		{"ApplySyncLeavesOtherConnectorsAlone", testApplySyncLeavesOtherConnectorsAlone},
		{"ApplySyncIsAllOrNothing", testApplySyncIsAllOrNothing},
		{"ApplySyncRequiresAKnownUser", testApplySyncRequiresAKnownUser},
		{"NonUUIDUserIDFindsNothing", testNonUUIDUserIDFindsNothing},
		{"ApplySyncStoresAnyValidJSONDefinitionExactly", testApplySyncStoresAnyValidJSONDefinitionExactly},
		{"GetToolMissingIsNotFound", testGetToolMissingIsNotFound},
		{"DeleteCatalogDropsOnlyThatConnectorsToolsAndRecord", testDeleteCatalogDropsOnlyThatConnectorsToolsAndRecord},
		{"ToolCountsCountsEachConnectorsCatalog", testToolCountsCountsEachConnectorsCatalog},
		// Embeddings.
		{"NewToolsArePendingInConnectorAndNameOrder", testNewToolsArePendingInConnectorAndNameOrder},
		{"SetEmbeddingsStoresVectorAndClearsPending", testSetEmbeddingsStoresVectorAndClearsPending},
		{"ChangedHashClearsEmbedding", testChangedHashClearsEmbedding},
		{"SetEmbeddingsSkipsStaleVectors", testSetEmbeddingsSkipsStaleVectors},
		{"PendingEmbeddingsRejectsNonPositiveLimit", testPendingEmbeddingsRejectsNonPositiveLimit},
		// Sync bookkeeping and opaque-byte persistence.
		{"ApplySyncKeepsTheLatestOutcomeAndHiddenToolsAcrossAFailure", testApplySyncKeepsTheLatestOutcomeAndHiddenToolsAcrossAFailure},
		{"CredentialRoundTripAndReplace", testCredentialRoundTripAndReplace},
		{"ListAndDeleteCredentials", testListAndDeleteCredentials},
		{"SecretRoundTripAndReplace", testSecretRoundTripAndReplace},
		{"DeleteSecret", testDeleteSecret},
		{"ListSecretsSortsTheUsersNames", testListSecretsSortsTheUsersNames},
		{"MetadataRoundTripAndReplace", testMetadataRoundTripAndReplace},
		{"HasSealedDataReportsCredentialAndSecretRows", testHasSealedDataReportsCredentialAndSecretRows},
		// Call log.
		{"RecordCallRoundTripsNewestFirstWithUUIDv7IDs", testRecordCallRoundTripsNewestFirstWithUUIDv7IDs},
		{"RecordCallKeepsMillisecondsOfStartedAt", testRecordCallKeepsMillisecondsOfStartedAt},
		{"ListCallsFilters", testListCallsFilters},
		{"ListCallsRejectsNonPositiveLimit", testListCallsRejectsNonPositiveLimit},
		{"PruneCallsDeletesOnlyTheUsersOlderCalls", testPruneCallsDeletesOnlyTheUsersOlderCalls},
		// Scoped client tokens.
		{"ClientTokensCreateListAndDelete", testClientTokensCreateListAndDelete},
		{"CreateClientTokenRefusesADuplicateNameOrDigest", testCreateClientTokenRefusesADuplicateNameOrDigest},
		{"CreateClientTokenRefusesNoConnectors", testCreateClientTokenRefusesNoConnectors},
		{"DeleteClientTokenMissingIsNotFound", testDeleteClientTokenMissingIsNotFound},
		{"TouchClientTokensNeverMovesBackwardsAndIgnoresUnknownDigests", testTouchClientTokensNeverMovesBackwardsAndIgnoresUnknownDigests},
		{"TouchClientTokensNeverCreditsANewTokenOfTheSameName", testTouchClientTokensNeverCreditsANewTokenOfTheSameName},
		// OAuth grants and tokens.
		{"OAuthGrantsCreateGetListAndDelete", testOAuthGrantsCreateGetListAndDelete},
		{"CreateOAuthGrantRefusesIncompleteGrants", testCreateOAuthGrantRefusesIncompleteGrants},
		{"OAuthGrantsAndClientTokensShareANameSpace", testOAuthGrantsAndClientTokensShareANameSpace},
		{"OAuthNameSpaceHoldsUnderRacingCreates", testOAuthNameSpaceHoldsUnderRacingCreates},
		{"IssueOAuthTokens", testIssueOAuthTokens},
		{"IssueOAuthTokensRefusesMalformedTokens", testIssueOAuthTokensRefusesMalformedTokens},
		{"RotateOAuthTokens", testRotateOAuthTokens},
		{"RotateOAuthTokensRefusesASpentRefreshToken", testRotateOAuthTokensRefusesASpentRefreshToken},
		{"RotateOAuthTokensUnknownRefreshTokenIsNotFound", testRotateOAuthTokensUnknownRefreshTokenIsNotFound},
		{"FailedRotateOAuthTokensLeavesTheRefreshTokenUnspent", testFailedRotateOAuthTokensLeavesTheRefreshTokenUnspent},
		{"RacingRotationsOfOneRefreshTokenHaveOneWinner", testRacingRotationsOfOneRefreshTokenHaveOneWinner},
		{"RotateOAuthTokensRefusesAZeroTime", testRotateOAuthTokensRefusesAZeroTime},
		{"RacingRotationAndGrantDeleteLeaveNoTokens", testRacingRotationAndGrantDeleteLeaveNoTokens},
		{"DeleteOAuthGrantDeletesItsTokens", testDeleteOAuthGrantDeletesItsTokens},
		{"ListOAuthAccessTokensKeepsOnlyUnexpiredAccessTokens", testListOAuthAccessTokensKeepsOnlyUnexpiredAccessTokens},
		{"TouchOAuthGrantsNeverMovesBackwardsAndIgnoresOtherDigests", testTouchOAuthGrantsNeverMovesBackwardsAndIgnoresOtherDigests},
		{"PruneOAuthDeletesExpiredSpentAndUnexchanged", testPruneOAuthDeletesExpiredSpentAndUnexchanged},
		{"PruneOAuthCountsEveryRowAndSparesOtherGrants", testPruneOAuthCountsEveryRowAndSparesOtherGrants},
		// Lexical search.
		{"SearchRanksNameMatchesAboveDescriptionMatches", testSearchRanksNameMatchesAboveDescriptionMatches},
		{"SearchReturnsFullCatalogEntries", testSearchReturnsFullCatalogEntries},
		{"SearchIsScopedToTheUser", testSearchIsScopedToTheUser},
		{"SearchWithNoMatchingTokensReturnsAnEmptyPage", testSearchWithNoMatchingTokensReturnsAnEmptyPage},
		{"SearchPagesPartitionTheRankingAndAreStable", testSearchPagesPartitionTheRankingAndAreStable},
		{"SearchHasMoreIsFalseWhenTheLastPageIsExactlyFull", testSearchHasMoreIsFalseWhenTheLastPageIsExactlyFull},
		{"SearchRejectsNonPositivePageOrSize", testSearchRejectsNonPositivePageOrSize},
		{"SearchConnectorFilterKeepsOnlyListedConnectors", testSearchConnectorFilterKeepsOnlyListedConnectors},
		{"SearchFoldsPluralsAndToleratesTyposInLongTokens", testSearchFoldsPluralsAndToleratesTyposInLongTokens},
		{"SearchCountsAPluralQueryWordOnce", testSearchCountsAPluralQueryWordOnce},
		{"SearchMatchesNonASCIILetters", testSearchMatchesNonASCIILetters},
		// Hybrid search.
		{"HybridSearchFusesLexicalAndVectorRanks", testHybridSearchFusesLexicalAndVectorRanks},
		{"HybridSearchExplainsEachResultsRanks", testHybridSearchExplainsEachResultsRanks},
		{"HybridSearchBreaksFusedTiesByQualifiedName", testHybridSearchBreaksFusedTiesByQualifiedName},
		{"HybridSearchComparesOnlyVectorsOfTheQueryModel", testHybridSearchComparesOnlyVectorsOfTheQueryModel},
		{"HybridSearchKeepsTheTop50VectorCandidatesAboveZero", testHybridSearchKeepsTheTop50VectorCandidatesAboveZero},
		{"HybridSearchConnectorFilterRestrictsTheFusedRanking", testHybridSearchConnectorFilterRestrictsTheFusedRanking},
		{"HybridSearchIgnoresVectorsOfAnotherLength", testHybridSearchIgnoresVectorsOfAnotherLength},
		{"HybridSearchRanksAnExactToolNameFirst", testHybridSearchRanksAnExactToolNameFirst},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, newDatabase) })
	}
}

// open opens a store on db and closes it when the test ends.
func open(t *testing.T, db Database) store.Store {
	t.Helper()
	s, err := db.Open(context.Background())

	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

// newStore opens a store on a fresh database and returns it with the local user.
func newStore(t *testing.T, newDatabase NewDatabase) (store.Store, string) {
	t.Helper()
	s := open(t, newDatabase(t))
	userID, err := s.EnsureUser(context.Background())

	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	return s, userID
}

// unknownUser returns a well-formed user ID that no store has created. User
// IDs are UUIDs, and a driver may reject anything else before it looks.
func unknownUser(t *testing.T) string {
	t.Helper()
	u, err := uuid.NewV7()

	if err != nil {
		t.Fatalf("new user ID: %v", err)
	}

	return u.String()
}
