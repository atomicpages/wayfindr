package clienttoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// DefaultName names the default client token, the one wayfindr init
// creates. It is reserved: no scoped token may take it.
const DefaultName = "default"

// DefaultPrefix starts the default client token value, so a leaked one is
// easy to recognize.
const DefaultPrefix = "wf_"

// ScopedPrefix starts every scoped client token value, so a scoped token is
// told apart from the default one at a glance and by secret scanners.
const ScopedPrefix = "wf_s_"

// Client is who presented a valid client token: the default token, which
// may use every connector, or a scoped token, which may use only its
// Connectors (docs/internal/plans/scoped-client-tokens.md).
type Client struct {
	// Name is DefaultName or the scoped token's name.
	Name string
	// Connectors are the connectors a scoped token may use; nil for the
	// default token, which may use every connector.
	Connectors []string
}

// Default is the Client of the default client token.
func Default() Client { return Client{Name: DefaultName} }

// Scoped reports whether c is a scoped token, limited to its Connectors.
func (c Client) Scoped() bool { return c.Connectors != nil }

// IsScoped reports whether value has the form of a scoped token. Such a
// value is never the default token, whatever source it came from.
func IsScoped(value string) bool { return strings.HasPrefix(value, ScopedPrefix) }

// Generate returns a new scoped token value: ScopedPrefix and the base64url
// of 32 random bytes.
func Generate() (string, error) { return generate(ScopedPrefix) }

// GenerateDefault returns a new default token value: DefaultPrefix and the
// base64url of 32 random bytes. About one draw in 4,096 begins "s_", which
// would read as a scoped token that the daemon refuses as the default
// (IsScoped), so such a draw is replaced.
func GenerateDefault() (string, error) {
	for {
		v, err := generate(DefaultPrefix)

		if err != nil || !IsScoped(v) {
			return v, err
		}
	}
}

func generate(prefix string) (string, error) {
	var b [32]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate client token: %w", err)
	}

	return prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Digest is the SHA-256 of a token value, the only form a token is stored
// or compared in.
func Digest(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

// tokenInfoKey is the TokenInfo.Extra key that carries the Client.
const tokenInfoKey = "wayfindr.client"

// TokenInfo is c as the MCP SDK's bearer middleware carries it into
// req.Extra.TokenInfo. UserID is c's name, for the SDK's own use; the
// gateway reads the Client back with FromTokenInfo.
func (c Client) TokenInfo() *auth.TokenInfo {
	return &auth.TokenInfo{UserID: c.Name, Extra: map[string]any{tokenInfoKey: Client{Name: c.Name, Connectors: slices.Clone(c.Connectors)}}}
}

// FromTokenInfo is the Client that ti carries; false when ti is nil or
// carries none, which a caller must refuse rather than serve unscoped.
func FromTokenInfo(ti *auth.TokenInfo) (Client, bool) {
	if ti == nil {
		return Client{}, false
	}

	c, ok := ti.Extra[tokenInfoKey].(Client)

	return c, ok
}
