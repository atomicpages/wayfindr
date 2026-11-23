package httpd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
)

// guards holds what every guard needs.
type guards struct {
	port int
	// ip is Config.IP, unmapped: the bound address, which a Host or Origin
	// may name as well as the three loopback names.
	ip netip.Addr
	// callback is Config.CallbackPath, the only path served to any remote
	// request; empty for none.
	callback string
	// tokenSum is the SHA-256 of the default client token. Comparing
	// digests keeps the comparison constant-time in the token's length too.
	tokenSum [sha256.Size]byte
	// clients resolves scoped client tokens; nil accepts only the default
	// token.
	clients *clienttoken.Registry
	log     zerolog.Logger
}

// host is the outermost guard. It runs before routing, so a remote request
// it refuses gets 404 for every path and method (never a 405 or a
// path-cleaning redirect), and a rebinding page or a scanner learns nothing
// about which routes exist. A request is remote when it carries a forwarding
// header or a Host that isn't loopback (see isRemote).
//
// A local request, and any request for the OAuth callback, goes on to the
// route's own guards. A remote request goes on only if admitRemote lets it,
// with the Client it resolved in the context for the route's guards.
func (gs guards) host(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (gs.callback != "" && r.URL.Path == gs.callback) || !gs.isRemote(r) {
			next.ServeHTTP(w, r)

			return
		}

		c, refused := gs.admitRemote(r)

		if refused != "" {
			gs.reject(r, refused)
			http.NotFound(w, r)

			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), remoteKey{}, c)))
	})
}

// isRemote reports whether r came through a proxy or tunnel, or names a Host
// other than a loopback name with the daemon's port. A forwarded request is
// remote whatever its Host says: a tunnel can rewrite Host to the loopback
// address, but it adds a forwarding header.
func (gs guards) isRemote(r *http.Request) bool {
	return forwarded(r) || !gs.isLoopbackHostPort(r.Host)
}

// admitRemote is the whole decision on a remote request other than one for
// the OAuth callback (ADR 0021): it may reach mcpPath, and nothing else,
// if and only if its bearer resolves, under the same one-header rule as a
// local request, to a remote scoped token. It returns that Client, or which
// check refused the request.
//
// The bearer is resolved before the path is looked at, on every path, so a
// refusal does the same work whatever the path and the token: a caller
// without a remote token can't tell from the answer, or its timing, whether
// mcpPath exists or how close its token was. host answers every refusal
// with the same 404.
func (gs guards) admitRemote(r *http.Request) (clienttoken.Client, string) {
	c, ok := gs.resolveBearer(r)

	switch {
	case r.URL.Path != mcpPath:
		return clienttoken.Client{}, "host"
	case !ok || !c.Scoped() || !c.Remote:
		return clienttoken.Client{}, "remote_token"
	}

	return c, ""
}

// remoteKey is the request context key under which host puts the Client of a
// remote request it admitted.
type remoteKey struct{}

// remoteFrom returns the Client host admitted ctx's remote request with;
// false for a local request.
func remoteFrom(ctx context.Context) (clienttoken.Client, bool) {
	c, ok := ctx.Value(remoteKey{}).(clienttoken.Client)

	return c, ok
}

// guard is the per-route part of design §7.7's guard column. The loopback
// Host check is not here: it runs before routing (see host).
type guard struct {
	// origin forbids a present, non-loopback Origin.
	origin bool
	// sameOrigin requires Origin to be exactly http://<Host>. A browser
	// always sends Origin on a form POST, so a missing one is refused.
	sameOrigin bool
	// bearer requires a client token, the default one or a scoped one, and
	// puts the Client it names in the request context (see clientFrom).
	bearer bool
	// adminOnly refuses a valid scoped token with 403; only the default
	// token passes. It needs bearer, which refuses anything else with 401.
	adminOnly bool
	// remote lets in a remote request that host admitted; any other route
	// answers one 404, as host does, so the route table and host's path
	// check must agree. It needs bearer, which then takes the Client host
	// resolved instead of resolving the token again.
	remote bool
}

// adminScopeMessage is the 403 body a scoped token gets on /admin/.
const adminScopeMessage = "This client token cannot use the admin API."

// wrap returns next behind the checks in g, run in table order.
func (gs guards) wrap(g guard, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote, isRemote := remoteFrom(r.Context())

		if isRemote && !g.remote {
			gs.reject(r, "remote_route")
			http.NotFound(w, r)

			return
		}

		if g.origin {
			if o, ok := r.Header["Origin"]; ok && (len(o) != 1 || !gs.isLoopbackOrigin(o[0])) {
				gs.reject(r, "origin")
				http.Error(w, "forbidden origin", http.StatusForbidden)

				return
			}
		}

		if g.sameOrigin {
			if o := r.Header.Values("Origin"); len(o) != 1 || o[0] != "http://"+r.Host {
				gs.reject(r, "same_origin")
				http.Error(w, "forbidden origin", http.StatusForbidden)

				return
			}
		}

		if g.bearer {
			c, ok := remote, isRemote

			if !isRemote {
				c, ok = gs.resolveBearer(r)
			}

			if !ok {
				gs.reject(r, "bearer")
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)

				return
			}

			if g.adminOnly && c.Scoped() {
				gs.reject(r, "admin_scope")
				http.Error(w, adminScopeMessage, http.StatusForbidden)

				return
			}

			r = r.WithContext(context.WithValue(r.Context(), clientKey{}, c))
		}

		next.ServeHTTP(w, r)
	})
}

// resolveBearer returns the Client that r's bearer names. r must carry
// exactly one Authorization header, "Bearer <token>" with the scheme
// case-insensitive, and the token must be the default one (compared in
// constant time) or be in the registry.
func (gs guards) resolveBearer(r *http.Request) (clienttoken.Client, bool) {
	vals := r.Header.Values("Authorization")

	if len(vals) != 1 {
		return clienttoken.Client{}, false
	}

	scheme, token, ok := strings.Cut(vals[0], " ")

	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return clienttoken.Client{}, false
	}

	sum := clienttoken.Digest(token)

	if subtle.ConstantTimeCompare(sum[:], gs.tokenSum[:]) == 1 {
		return clienttoken.Default(), true
	}

	if gs.clients == nil {
		return clienttoken.Client{}, false
	}

	// The registry holds only scoped tokens (Registry.Add), so a hit never
	// passes for the default token.
	return gs.clients.Lookup(sum)
}

// clientKey is the request context key under which the bearer guard puts the
// Client it resolved.
type clientKey struct{}

// clientFrom returns the Client the bearer guard resolved for ctx's request.
func clientFrom(ctx context.Context) (clienttoken.Client, bool) {
	c, ok := ctx.Value(clientKey{}).(clienttoken.Client)

	return c, ok
}

// authorizationKey is the request context key under which carryClient keeps
// the request's own Authorization values while the SDK middleware runs.
type authorizationKey struct{}

// sdkBearer is the Authorization value carryClient shows the SDK middleware.
// The bearer guard has already resolved the real one; this one only has to
// parse, so the SDK's looser header rules (Header.Get, strings.Fields) never
// refuse a request the guard accepted, such as a default token with inner
// spaces.
const sdkBearer = "Bearer resolved-by-httpd"

// carryClient runs next behind the go-sdk's bearer middleware, whose only job
// here is to put the Client the bearer guard resolved into the request's
// auth.TokenInfo: the SDK reads it through an unexported context key and
// hands it to the gateway as req.Extra.TokenInfo. The gateway is stateless,
// so no session outlives a request: each request is narrowed by its own
// token. The guard stays the authority: the verifier resolves no token
// itself, and the middleware never sees the real header.
func carryClient(next http.Handler) http.Handler {
	verify := func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		c, ok := clientFrom(ctx)

		if !ok {
			return nil, fmt.Errorf("httpd: no client resolved: %w", auth.ErrInvalidToken)
		}

		return c.TokenInfo(), nil
	}
	sdk := auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if v, ok := r.Context().Value(authorizationKey{}).([]string); ok {
				r.Header["Authorization"] = v
			}

			next.ServeHTTP(w, r)
		}))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Clone copies the header map, so the swap and the restore above
		// touch only this request's copy.
		r = r.Clone(context.WithValue(r.Context(), authorizationKey{}, r.Header.Values("Authorization")))
		r.Header.Set("Authorization", sdkBearer)
		sdk.ServeHTTP(w, r)
	})
}

// isLoopbackOrigin reports whether origin is http(s) on a loopback name, on
// any port. A missing port is allowed: the scheme default is still loopback.
func (gs guards) isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)

	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}

	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}

	return gs.isLoopbackName(u.Hostname())
}

// reject logs a refused request. Only the method, the path (never the query),
// and which guard refused it are logged; header values never are (design §9).
func (gs guards) reject(r *http.Request, guard string) {
	gs.log.Debug().Str("method", r.Method).Str("path", r.URL.Path).
		Str("guard", guard).Msg("request rejected")
}

// forwarded reports whether r carries any header a proxy or tunnel adds to
// say whom it forwards for, even an empty one: the standard Forwarded and
// Via, the de facto X-Forwarded-For, -Host, -Proto, and X-Real-IP, and
// Cloudflare's. A local client sends none of them. Via is on the list though
// some proxies leave it out and a local debugging proxy may add it: a local
// request taken for remote fails closed, a remote one taken for local fails
// open (ADR 0021).
func forwarded(r *http.Request) bool {
	for _, h := range [...]string{
		"Forwarded", "Via", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
		"X-Real-Ip", "Cf-Connecting-Ip",
	} {
		if _, ok := r.Header[h]; ok {
			return true
		}
	}

	return false
}

// isLoopbackHostPort reports whether hostport is a loopback name with
// exactly the port the daemon listens on.
func (gs guards) isLoopbackHostPort(hostport string) bool {
	host, p, err := net.SplitHostPort(hostport)

	if err != nil || p != strconv.Itoa(gs.port) {
		return false
	}

	return gs.isLoopbackName(host)
}

// isLoopbackName reports whether host is one of the three loopback names the
// design allows, or the IP literal the daemon is bound to (such as
// 127.0.0.2). It compares names, never resolving them: resolving is what DNS
// rebinding exploits, and an IP literal can't be rebound.
func (gs guards) isLoopbackName(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}

	if !gs.ip.IsValid() || !gs.ip.IsLoopback() {
		return false
	}

	ip, err := netip.ParseAddr(host)

	return err == nil && ip == gs.ip
}
