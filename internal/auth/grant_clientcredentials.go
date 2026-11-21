package auth

import (
	"context"
	"net/url"
	"strings"
)

// clientCredentials is the client_credentials grant (RFC 6749 §4.4): the
// OAuth client's own access, so the request carries nothing but the scope.
// The client proves who it is through its client authentication, which
// [chooseClientAuth] picks and [oauthClient.exchange] applies.
func clientCredentials(_ context.Context, _ oauthClient, scopes []string) (url.Values, error) {
	form := url.Values{"grant_type": {"client_credentials"}}

	if len(scopes) > 0 {
		form.Set("scope", strings.Join(scopes, " "))
	}

	return form, nil
}
