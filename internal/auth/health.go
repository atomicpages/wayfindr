package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/atomicpages/wayfindr/internal/config"
)

// How far ahead a predictable credential failure is warned of (GLOSSARY:
// Warning). Refresh stays lazy (ADR 0004): a warning only reads.
const (
	// warnSoon is how close a failure must be to be urgent: the agent sees
	// it in search_tools, and a token that cannot be refreshed turns from
	// a note into a warning.
	warnSoon = 24 * time.Hour
	// warnAhead is how far ahead a refresh token's or a registered
	// client's end is warned of.
	warnAhead = 7 * 24 * time.Hour
)

// WarningCode names what a [Warning] predicts.
type WarningCode string

// The credential warnings.
const (
	// WarningNoRefresh: the access token expires and there is no refresh
	// token, so the connector will need authorizing again.
	WarningNoRefresh WarningCode = "no_refresh"
	// WarningRefreshExpiring: the refresh token stops working within
	// warnAhead, or has.
	WarningRefreshExpiring WarningCode = "refresh_expiring"
	// WarningRegistrationExpiring: the DCR client the token is refreshed
	// with expires within warnAhead, or has.
	WarningRegistrationExpiring WarningCode = "registration_expiring"
)

// Health is what a connector's stored credential predicts about its
// future, as [Broker.Health] reads it. Nothing of the token leaves the
// broker but these facts.
type Health struct {
	// AccessExpiry is when the access token expires; zero when the server
	// sent no expires_in.
	AccessExpiry time.Time
	// HasRefresh reports whether there is a refresh token.
	HasRefresh bool
	// RefreshExpiry is when the refresh token stops working; zero when the
	// server did not say.
	RefreshExpiry time.Time
	// RegistrationExpiry is when the DCR client a refresh would use
	// expires; zero when it never does, or a refresh would use another
	// client.
	RegistrationExpiry time.Time
}

// Warning is a credential failure Health predicts: what will fail, when,
// and the advice that heads it off.
type Warning struct {
	Code WarningCode
	// At is when the connector breaks.
	At time.Time
	// Note is set on a token that cannot be refreshed but has more than a
	// day left: the user should know, but nothing is urgent yet.
	Note bool
	Advice
}

// Urgent reports whether w is due within a day of now and is not a note:
// the agent is told of it, so it can tell the user before a call fails.
func (w Warning) Urgent(now time.Time) bool {
	return !w.Note && w.At.Sub(now) <= warnSoon
}

// Health reads what connector c's stored credential predicts; c is the
// connector's current config (the broker keeps no copy). It is zero under
// the rules a token is not sent by: when there is no token, when the token
// cannot be opened, and when it is bound to a url other than c's
// (wayfindr-zgp.14; the connector's own state then says why). It is zero
// too for a connector that does not use OAuth, and one whose url does not
// resolve. It is zero for an unattended grant, whose token is held in
// memory and got again whenever it nears expiry, so nothing ahead can fail
// for want of a refresh. Errors are [*Error]s of [CaseStore].
func (b *Broker) Health(ctx context.Context, c config.Connector) (Health, error) {
	if !c.UsesOAuth() || c.Auth.Grant.Unattended() {
		return Health{}, nil
	}

	resource, err := c.URL.Resolve(ctx, b.resolver)

	if err != nil {
		return Health{}, nil
	}

	v, err := b.creds.load(ctx, c.Name, resource, *c.Auth)

	if e, ok := AsError(err); ok && e.Case == CaseStore {
		return Health{}, err
	}

	if err != nil {
		return Health{}, nil
	}

	h := Health{
		AccessExpiry:  v.Token.Expiry,
		HasRefresh:    v.Token.Refresh != "",
		RefreshExpiry: v.Token.RefreshExpiry,
	}

	// A refresh uses the stored DCR client only when the config names no
	// client (ADR 0007). With client_metadata_url set it may use CIMD,
	// which only discovery can tell, so no expiry is claimed then.
	if a := c.Auth; a.ClientID == "" && a.ClientMetadataURL == "" && v.Client != nil {
		h.RegistrationExpiry = v.Client.SecretExpiry
	}

	return h, nil
}

// Warnings returns what h, connector's health, predicts as of now. A
// token that cannot be refreshed is always reported, as a note until a day
// before it expires; a refresh token's or a registered client's end is
// reported from warnAhead before it. Ends already past are reported too.
// The warnings that authorizing again fixes carry link, the connector's
// auth link (see [Broker.Link]).
func (h Health) Warnings(now time.Time, connector, link string) []Warning {
	var out []Warning

	if !h.AccessExpiry.IsZero() && !h.HasRefresh {
		w := Warning{Code: WarningNoRefresh, At: h.AccessExpiry, Note: h.AccessExpiry.Sub(now) > warnSoon}
		w.Advice = ending(now, h.AccessExpiry, link, "The access token %s at %s and cannot be refreshed.", beforeReauth, reauth)
		out = append(out, w)
	}

	if h.HasRefresh && within(now, h.RefreshExpiry) {
		w := Warning{Code: WarningRefreshExpiring, At: h.RefreshExpiry}
		w.Advice = ending(now, h.RefreshExpiry, link, "The refresh token %s at %s.", beforeReauth, reauth)
		out = append(out, w)
	}

	if within(now, h.RegistrationExpiry) {
		w := Warning{Code: WarningRegistrationExpiring, At: h.RegistrationExpiry}
		// An auth flow reuses the stored client while it is there, so the
		// auth link alone would not help: a revoke deletes the client with
		// the token, and the next flow registers a new one.
		revoke := "`wayfindr auth logout " + connector + "`, then authorize again: Wayfindr registers a new client."
		w.Advice = ending(now, h.RegistrationExpiry, "", "The OAuth client Wayfindr registered for the connector %s at %s.",
			"Before then, run "+revoke, "Run "+revoke)
		out = append(out, w)
	}

	return out
}

const (
	beforeReauth = "Authorize again before then to avoid an interruption."
	reauth       = "Open the link to authorize the connector again."
)

// within reports whether t is set and no more than warnAhead after now.
func within(now, t time.Time) bool {
	return !t.IsZero() && t.Sub(now) <= warnAhead
}

// ending is the advice for something that ends at t: summary, a format
// with the verb ("expires" or "expired") and the time, and the action that
// heads it off, or once t has passed, the one that fixes it.
func ending(now, t time.Time, link, summary, ahead, past string) Advice {
	verb, action := "expires", ahead

	if !now.Before(t) {
		verb, action = "expired", past
	}

	return Advice{Summary: fmt.Sprintf(summary, verb, t.UTC().Format(time.RFC3339)), Action: action, Link: link}
}
