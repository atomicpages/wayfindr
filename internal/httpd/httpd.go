// Package httpd owns the daemon's HTTP listener, its route table, and the
// Host, Origin, and bearer guards in front of every route (design §7.7).
//
// The handlers behind the routes belong to other packages (gateway, auth,
// the admin API) and are injected through Handlers; httpd decides only who
// may reach them. The admin API is one handler for the whole /admin/ tree,
// which owns its own routes (package admin).
package httpd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"path"
	"slices"
	"strings"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
)

// Handlers are the route handlers other packages provide. Each runs only
// after its route's guards pass, and can read its path values with
// r.PathValue. A nil handler answers 501 Not Implemented (still behind its
// guards), so the daemon can wire routes as their packages land.
type Handlers struct {
	// MCP serves POST, GET, and DELETE /mcp: the gateway. It runs behind
	// the go-sdk's auth.RequireBearerToken, so auth.TokenInfoFromContext
	// (and an MCP request's Extra.TokenInfo) carries the caller's
	// clienttoken.Client; read it with clienttoken.FromTokenInfo.
	MCP http.Handler
	// AuthConfirm serves GET /auth/{connector}: the "Continue" page.
	AuthConfirm http.Handler
	// AuthStart serves POST /auth/{connector}: starts the flow. httpd
	// checks the same-origin Origin; the handler checks the form nonce.
	AuthStart http.Handler
	// OAuthCallback serves GET Config.CallbackPath: auth's
	// Flows.HandleCallback.
	OAuthCallback http.Handler
	// Admin serves every path under /admin/: admin.New, which routes them
	// itself. httpd only guards them; only the default client token gets
	// through.
	Admin http.Handler
	// UI serves every path under /ui/: the web console (webui.New), which
	// routes them itself and checks its own session cookie (ADR 0016).
	// httpd lets a GET through on a loopback Origin and a POST only on the
	// same-origin Origin, as for /auth/{connector}.
	UI http.Handler

	// The authorization server's routes (ADR 0022, design §7.11), behind
	// the public guard: no Origin check and no bearer. A local request
	// always reaches them; a remote one only while
	// Config.ResourceMetadataURL returns a URL, and only with the route's
	// own method. The handlers answer 404 while [server.oauth] is off.

	// OAuthResourceMetadata serves GET /.well-known/oauth-protected-resource
	// and GET /.well-known/oauth-protected-resource/mcp.
	OAuthResourceMetadata http.Handler
	// OAuthServerMetadata serves GET /.well-known/oauth-authorization-server.
	OAuthServerMetadata http.Handler
	// OAuthAuthorize serves GET /oauth/authorize.
	OAuthAuthorize http.Handler
	// OAuthToken serves POST /oauth/token.
	OAuthToken http.Handler
}

// Config is what NewHandler needs.
type Config struct {
	// Port is the port the daemon listens on. A Host header names a loopback
	// address only with this port. NewServer fills it from the listener.
	Port int
	// IP is the loopback address the daemon is bound to. Besides the three
	// loopback names, a Host or Origin may name this IP literal, so a
	// listen address such as 127.0.0.2 is reachable by its own address. An
	// IP literal is never resolved, so it can't be rebound. NewServer fills
	// it from the listener; the zero Addr, or one that isn't loopback, adds
	// nothing.
	IP netip.Addr
	// Token is the default client token, which clients present as a bearer
	// and which may use every route and every connector. Read it with
	// clienttoken.Read. It must not be empty.
	Token string
	// Clients resolves scoped client tokens, which may use /mcp (limited to
	// their connectors by the gateway) but not /admin/. Nil accepts only
	// Token. httpd reads it on every request, so an Add or Remove applies to
	// the next one.
	Clients *clienttoken.Registry
	// Version is reported by /healthz.
	Version string
	// Logger receives rejected-request events.
	Logger zerolog.Logger
	// CallbackPath is the path of the OAuth callback (auth.CallbackPath),
	// the only route served on any Host: the authorization server
	// redirects the browser there, possibly by another name for this
	// machine (ADR 0007). Empty serves no callback route.
	CallbackPath string
	// Handlers are the injected route handlers.
	Handlers Handlers
	// ResourceMetadataURL returns the URL a remote /mcp request's 401 names
	// while Wayfindr's authorization server is on, and "" while it is off
	// (ADR 0022). Built from the canonical issuer, its host is the issuer's:
	// only a remote request whose Host is that host, or a loopback one with
	// a forwarding header, gets the public admission and the 401. host
	// calls it once per remote request, so a reload applies from the next
	// one. Nil means always off.
	ResourceMetadataURL func() string
}

// mcpPath is the MCP endpoint, the one route a remote request reaches with
// a token (ADR 0021).
const mcpPath = "/mcp"

// The authorization server's paths. httpd can't import oauthserver
// (design §2), so it names them itself; a daemon test
// (TestOAuthPathsAgree) asserts each equals oauthserver's constant.
const (
	ResourceMetadataPath = "/.well-known/oauth-protected-resource"
	ServerMetadataPath   = "/.well-known/oauth-authorization-server"
	AuthorizePath        = "/oauth/authorize"
	TokenPath            = "/oauth/token" //nolint:gosec // G101: a path, not a credential
)

// NewHandler returns the guarded route table.
func NewHandler(cfg Config) (http.Handler, error) {
	if cfg.Token == "" {
		return nil, errors.New("httpd: empty client token")
	}

	if cfg.CallbackPath != "" && !strings.HasPrefix(cfg.CallbackPath, "/") {
		return nil, errors.New("httpd: the callback path must start with /")
	}

	// The callback is served on any Host with no guard, so it may not take
	// (or sit under) a path another route owns: the route would be served
	// to a remote request unguarded, or the mux would refuse the pattern.
	if reservedPath(cfg.CallbackPath) {
		return nil, errors.New("httpd: the callback path is one a route already serves (/mcp, /admin/, /ui/, /healthz, /auth/, or the authorization server's)")
	}

	h := cfg.Handlers
	// The route table of design §7.7. Every route but the callback also
	// requires a loopback Host and no forwarding header; that check wraps
	// the whole mux below. The one exception is /mcp, which a remote
	// request reaches with a remote scoped token (ADR 0021), and the
	// authorization server's five routes, which a remote request reaches
	// with no token while it is on (ADR 0022).
	//
	//	route                  guards (after Host)      who passes
	//	GET /healthz           origin                   anyone local
	//	POST|GET|DELETE /mcp   origin, bearer, remote   default or scoped token; remote: a remote token
	//	GET /auth/{connector}  origin                   anyone local
	//	POST /auth/{connector} origin, same-origin      the confirmation form
	//	/admin/                origin, bearer, admin    default token only (scoped: 403)
	//	GET /ui/               origin                   anyone local (the console checks its session)
	//	POST /ui/              origin, same-origin      the console's own forms
	//	GET <CallbackPath>     none, on any Host        the OAuth redirect
	//	GET <ResourceMetadataPath>[/mcp], GET <ServerMetadataPath>,
	//	GET <AuthorizePath>, POST <TokenPath>
	//	                       public                   anyone local; remote only while on
	//
	// /mcp also runs behind the go-sdk bearer middleware (carryClient), which
	// carries the resolved Client into the request's auth.TokenInfo.
	loopback := guard{origin: true}
	mcpGuard := guard{origin: true, bearer: true, remote: true}
	admin := guard{origin: true, bearer: true, adminOnly: true}
	public := guard{public: true}
	mcp := carryClient(orNotImplemented(h.MCP))
	type route struct {
		pattern string
		guard   guard
		handler http.Handler
	}
	routes := []route{
		{"GET /healthz", loopback, healthz(cfg.Version)},
		{"POST " + mcpPath, mcpGuard, mcp},
		{"GET " + mcpPath, mcpGuard, mcp},
		{"DELETE " + mcpPath, mcpGuard, mcp},
		{"GET /auth/{connector}", loopback, h.AuthConfirm},
		{"POST /auth/{connector}", guard{origin: true, sameOrigin: true}, h.AuthStart},
		{"/admin/", admin, h.Admin},
		{"GET /ui/", loopback, h.UI},
		{"POST /ui/", guard{origin: true, sameOrigin: true}, h.UI},
	}

	// A GET pattern also serves HEAD, which is why a public route lists it
	// as a method without a pattern of its own.
	for _, pr := range publicRoutes() {
		routes = append(routes, route{pr.methods[0] + " " + pr.path, public, pr.handler(h)})
	}

	if cfg.CallbackPath != "" {
		routes = append(routes, route{"GET " + cfg.CallbackPath, guard{}, h.OAuthCallback})
	}

	gs := guards{
		port:                cfg.Port,
		ip:                  cfg.IP.Unmap(),
		callback:            cfg.CallbackPath,
		tokenSum:            clienttoken.Digest(cfg.Token),
		clients:             cfg.Clients,
		resourceMetadataURL: cfg.ResourceMetadataURL,
		log:                 cfg.Logger,
	}
	mux := http.NewServeMux()

	for _, rt := range routes {
		mux.Handle(rt.pattern, gs.wrap(rt.guard, orNotImplemented(rt.handler)))
	}

	return gs.host(mux), nil
}

// publicRoute is one of the authorization server's routes, which a remote
// request reaches with no token while the server is on. The route table and
// host's admission both read this one list, so they cannot disagree.
type publicRoute struct {
	path string
	// methods are the methods a remote request may use. The first is the
	// route's pattern; a GET pattern serves HEAD as well.
	methods []string
	handler func(Handlers) http.Handler
}

// publicRoutes lists the authorization server's routes: GET and HEAD on the
// three metadata paths, GET on AuthorizePath, POST on TokenPath. A remote
// HEAD of AuthorizePath gets the 404, so a link unfurler never reaches the
// authorize handler.
func publicRoutes() []publicRoute {
	getHead := []string{http.MethodGet, http.MethodHead}

	return []publicRoute{
		{ResourceMetadataPath, getHead, func(h Handlers) http.Handler { return h.OAuthResourceMetadata }},
		{ResourceMetadataPath + mcpPath, getHead, func(h Handlers) http.Handler { return h.OAuthResourceMetadata }},
		{ServerMetadataPath, getHead, func(h Handlers) http.Handler { return h.OAuthServerMetadata }},
		{AuthorizePath, []string{http.MethodGet}, func(h Handlers) http.Handler { return h.OAuthAuthorize }},
		{TokenPath, []string{http.MethodPost}, func(h Handlers) http.Handler { return h.OAuthToken }},
	}
}

// publicMethods returns the methods a remote request may use on path while
// the authorization server is on, and none (nil) on any other path. The path
// is compared exactly, so one the mux would clean or redirect is not public.
func publicMethods(path string) []string {
	for _, pr := range publicRoutes() {
		if pr.path == path {
			return pr.methods
		}
	}

	return nil
}

// reservedPath reports whether p is a path some route owns, or lies under
// one: the callback path may not be.
func reservedPath(p string) bool {
	roots := []string{mcpPath, "/healthz", "/auth", "/admin", "/ui"}

	for _, pr := range publicRoutes() {
		roots = append(roots, pr.path)
	}

	p = path.Clean(p)

	return slices.ContainsFunc(roots, func(root string) bool {
		return p == root || strings.HasPrefix(p, root+"/")
	})
}

func healthz(version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Version string `json:"version"`
		}{version})
	})
}

func orNotImplemented(h http.Handler) http.Handler {
	if h != nil {
		return h
	}

	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})
}
