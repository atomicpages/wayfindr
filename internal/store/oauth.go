package store

import (
	"errors"
	"fmt"
	"time"
)

// CheckOAuthGrant reports why CreateOAuthGrant must refuse g before it
// touches the database, or nil. Both drivers call it, so they refuse the same
// grants.
func CheckOAuthGrant(g OAuthGrant) error {
	switch {
	case len(g.Connectors) == 0:
		return errors.New("no connectors")
	case g.ClientID == "":
		return errors.New("no client ID")
	case g.Resource == "":
		return errors.New("no resource")
	case g.CreatedAt.IsZero():
		return errors.New("no creation time")
	}

	return nil
}

// CheckOAuthRotation reports why RotateOAuthTokens must refuse the rotation
// before it touches the database, or nil: CheckOAuthTokenPair, and a zero at,
// which would store a spent token with no spend time. Both drivers call it.
func CheckOAuthRotation(userID string, at time.Time, access, refresh OAuthToken) error {
	if at.IsZero() {
		return errors.New("no rotation time")
	}

	return CheckOAuthTokenPair(userID, access, refresh)
}

// CheckOAuthTokenPair reports why IssueOAuthTokens or RotateOAuthTokens must
// refuse access and refresh before it touches the database, or nil: the
// kinds, the expiry only an access token has, no UsedAt, a creation time, one
// grant, and a user ID that is empty or userID. Both drivers call it.
func CheckOAuthTokenPair(userID string, access, refresh OAuthToken) error {
	switch {
	case access.Kind != OAuthAccess:
		return fmt.Errorf("access token is kind %q", access.Kind)
	case refresh.Kind != OAuthRefresh:
		return fmt.Errorf("refresh token is kind %q", refresh.Kind)
	case access.ExpiresAt == nil:
		return errors.New("access token has no expiry")
	case refresh.ExpiresAt != nil:
		return errors.New("refresh token has an expiry")
	case access.UsedAt != nil || refresh.UsedAt != nil:
		return errors.New("a new token is already spent")
	case access.CreatedAt.IsZero() || refresh.CreatedAt.IsZero():
		return errors.New("no creation time")
	case access.Grant == "" || access.Grant != refresh.Grant:
		return errors.New("the tokens name different grants")
	case access.Digest == refresh.Digest:
		return errors.New("the tokens have one digest")
	case (access.UserID != "" && access.UserID != userID) || (refresh.UserID != "" && refresh.UserID != userID):
		return errors.New("a token belongs to another user")
	}

	return nil
}
