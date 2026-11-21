package auth

import "context"

// Revoke forgets the connector's credential (design §7.3): it cancels the
// connector's pending auth flows, those still starting too, and deletes
// its credentials row, both
// under the row lock, so a flow's callback either stored its token before
// (and the delete removes it) or stores nothing. It then reports the
// change (OnCredentialChanged), which puts a configured connector in
// needs_auth. When there was no credential it reports false and reports no
// change, but its pending flows are still cancelled: a flow the user
// started before revoking must not store a credential after it.
//
// A write racing the revoke cannot bring the row back: only a new grant
// and a flow's client registration start a row (see credentials), and
// a refresh, a step-up, or a forgotten DCR client that finds no row stores
// nothing and fails no_credential.
//
// The whole row goes, the DCR client with the token: the user asked
// Wayfindr to forget the connector's authorization, a row left with only a
// client would still be listed as an orphaned credential, and the next
// auth flow registers a new client. Nothing is sent to the authorization
// server (no RFC 7009 revocation in v1). The connector need not be in the
// config: an orphaned credential is revoked the same way.
//
// A connector with an unattended grant has no row, but its token is held
// in memory: Revoke forgets it, and counts it as found when the grant ever
// ran, so the change it reports reconnects the connector and the grant
// runs again. That is how a user retries an unattended grant the provider
// refused, once it is fixed there. It still deletes any row such a
// connector was left with (a hand edit from an auth flow's grant).
//
// Errors are [*Error]s of [CaseStore].
func (b *Broker) Revoke(ctx context.Context, connector string) (bool, error) {
	held := b.unattended.forget(b.userID + "|" + connector)
	found, err := b.creds.delete(ctx, connector, func() {
		b.cancelFlowsLocked(connector)
	})

	if err != nil {
		return false, err
	}

	if found = found || held; !found {
		return false, nil
	}

	b.log.Info().Str("connector", connector).Msg("oauth credential revoked")
	b.credentialChanged(connector)

	return true, nil
}

// cancelFlowsLocked ends the connector's pending flows, starting or not,
// as failed with [CaseFlowCancelled], so a late callback exchanges no code
// and stores no token, and a device code flow's poller stops. Revoke holds the row lock, so a callback storing its
// token now either finished first (and the revoke deletes it) or stores
// nothing.
func (b *Broker) cancelFlowsLocked(connector string) {
	err := b.fail(CaseFlowCancelled, connector, "the connector's credential was revoked", "", nil)

	if n := b.flows.cancel(connector, err); n > 0 {
		b.log.Info().Str("connector", connector).Int("flows", n).Msg("oauth flows cancelled")
	}
}
