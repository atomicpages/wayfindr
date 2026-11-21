// Package clienttoken reads the client token that clients present as a bearer
// to the daemon (design §3). The daemon, the stdio shim, and httpd all need
// the same env-over-file rule, so it lives here, below all of them.
package clienttoken

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/atomicpages/wayfindr/internal/paths"
)

// EnvVar holds the client token value. When set it takes precedence over
// <data>/client-token, for CI and containers (design §3).
const EnvVar = "WAYFINDR_CLIENT_TOKEN"

// ErrEmpty means the selected source holds only whitespace. An empty token
// would let any bearer through, so it is an error rather than a value.
var ErrEmpty = errors.New("client token is empty")

// Read returns the client token: the value of EnvVar when it is set,
// otherwise the contents of p.ClientTokenFile(). Surrounding whitespace is
// trimmed from either source. A missing file with no env value returns an
// error matching fs.ErrNotExist.
func Read(ctx context.Context, env paths.Env, p paths.Paths) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if v := env.Getenv(EnvVar); v != "" {
		return nonEmpty(v, EnvVar)
	}

	path := p.ClientTokenFile()
	b, err := os.ReadFile(path) //nolint:gosec // path comes from paths.Resolve, not from a request

	if err != nil {
		return "", fmt.Errorf("read client token %s: %w", path, err)
	}

	return nonEmpty(string(b), path)
}

func nonEmpty(v, source string) (string, error) {
	if t := strings.TrimSpace(v); t != "" {
		return t, nil
	}

	return "", fmt.Errorf("%s: %w", source, ErrEmpty)
}

// WithoutScoped returns env, in os.Environ form, without an EnvVar entry
// whose trimmed value IsScoped; env itself is not modified. A client running
// `wayfindr stdio` with its scoped token in EnvVar may be the one that starts
// the daemon, which must read its default token instead, from the file. The
// daemon refuses a scoped value outright; dropping it here lets that client
// still start the daemon.
func WithoutScoped(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		v, ok := strings.CutPrefix(kv, EnvVar+"=")

		return ok && IsScoped(strings.TrimSpace(v))
	})
}
