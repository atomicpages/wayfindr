package auth

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"strings"

	"github.com/atomicpages/wayfindr/internal/config"
)

// jwtBearerGrantType is the grant_type of the JWT bearer grant (RFC 7523
// §2.1).
const jwtBearerGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer" //nolint:gosec // G101: a URN, not a credential

// jwtBearer is the JWT bearer grant (RFC 7523 §2.1) with its assertion
// from src (GLOSSARY: Assertion): a workload token read from a file, a
// value resolved through resolver, or one Wayfindr signs itself. The
// assertion is obtained for every grant and never kept, so a token
// rotated on disk or in the environment is used by the next grant. A
// refusal is final: there is no retry with the same assertion, and no
// other grant to fall back to.
func jwtBearer(src *config.Assertion, resolver config.Resolver) unattendedGrant {
	return func(ctx context.Context, oc oauthClient, scopes []string) (url.Values, error) {
		assertion, err := obtainAssertion(ctx, oc, src, resolver)

		if err != nil {
			return nil, err
		}

		form := url.Values{"grant_type": {jwtBearerGrantType}, "assertion": {assertion}}

		if len(scopes) > 0 {
			form.Set("scope", strings.Join(scopes, " "))
		}

		return form, nil
	}
}

// obtainAssertion returns the assertion src names. A file that cannot be
// read, or a value that does not resolve, or either one empty, is
// [CaseUnresolvedValue]; a self-signed assertion that cannot be signed is
// [CaseSigningKeyInvalid]. No error quotes the assertion.
func obtainAssertion(ctx context.Context, oc oauthClient, src *config.Assertion, resolver config.Resolver) (string, error) {
	switch {
	case src == nil: // validation requires the table
		return "", oc.fail(CaseUnresolvedValue, "the jwt_bearer grant has no [auth.assertion] table", "", nil)
	case src.File != "":
		b, err := os.ReadFile(src.File)

		if err != nil {
			return "", oc.fail(CaseUnresolvedValue, "assertion.file", "", err)
		}

		return nonEmpty(oc, "assertion.file "+src.File, string(b))
	case src.Value != "":
		v, err := src.Value.Resolve(ctx, resolver)

		if err != nil {
			return "", oc.fail(CaseUnresolvedValue, "assertion.value", "", err)
		}

		return nonEmpty(oc, "assertion.value", v)
	case src.Subject != "":
		return selfSignedAssertion(oc, src.Subject)
	}

	return "", oc.fail(CaseUnresolvedValue, "the [auth.assertion] table names no source", "", nil)
}

// nonEmpty returns v without surrounding whitespace (a file's trailing
// newline), or a [CaseUnresolvedValue] error about what when nothing is
// left.
func nonEmpty(oc oauthClient, what, v string) (string, error) {
	if v = strings.TrimSpace(v); v == "" {
		return "", oc.fail(CaseUnresolvedValue, what+" is empty", "", nil)
	}

	return v, nil
}

// selfSignedAssertion is an assertion Wayfindr signs with the connector's
// signing key (the registration's AssertionKey) for subject: issued by the
// client, for the authorization server as signing_key.audience says, with
// a fresh jti, and a minute to live.
func selfSignedAssertion(oc oauthClient, subject string) (string, error) {
	k := oc.registration.AssertionKey

	if k == nil { // validation requires signing_key with subject
		return "", oc.fail(CaseSigningKeyInvalid, "assertion.subject needs signing_key", "", nil)
	}

	var nonce [16]byte

	if _, err := rand.Read(nonce[:]); err != nil {
		return "", oc.fail(CaseSigningKeyInvalid, "assertion jti", "", err)
	}

	now := oc.now()
	jwt, err := signJWS(jwsHeader{Alg: pickAlg(k.Signer, k.Alg, nil), Typ: "JWT", KID: k.KID}, assertionClaims{
		Iss: oc.registration.ClientID, Sub: subject, Aud: assertionAudience(oc.disc, k.Audience),
		JTI: b64url(nonce[:]), IAT: now.Unix(), Exp: now.Add(assertionLifetime).Unix(),
	}, k.Signer)

	if err != nil {
		return "", oc.fail(CaseSigningKeyInvalid, "sign the assertion", "", err)
	}

	return jwt, nil
}
