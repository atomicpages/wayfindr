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
}

// NewHandler returns the guarded route table.
func NewHandler(cfg Config) (http.Handler, error) {
	if cfg.Token == "" {
		return nil, errors.New("httpd: empty client token")
	}

	if cfg.CallbackPath != "" && !strings.HasPrefix(cfg.CallbackPath, "/") {
		return nil, errors.New("httpd: the callback path must start with /")
	}

	h := cfg.Handlers
	// The route table of design §7.7. Every route but the callback also
	// requires a loopback Host; that check wraps the whole mux below.
	//
	//	route                  guards (after Host)      who passes
	//	GET /healthz           origin                   anyone local
	//	POST|GET|DELETE /mcp   origin, bearer           default or scoped token
	//	GET /auth/{connector}  origin                   anyone local
	//	POST /auth/{connector} origin, same-origin      the confirmation form
	//	/admin/                origin, bearer, admin    default token only (scoped: 403)
	//	GET /ui/               origin                   anyone local (the console checks its session)
	//	POST /ui/              origin, same-origin      the console's own forms
	//	GET <CallbackPath>     none, on any Host        the OAuth redirect
	//
	// /mcp also runs behind the go-sdk bearer middleware (carryClient), which
	// carries the resolved Client into the request's auth.TokenInfo.
	loopback := guard{origin: true}
	bearer := guard{origin: true, bearer: true}
	admin := guard{origin: true, bearer: true, adminOnly: true}
	mcp := carryClient(orNotImplemented(h.MCP))
	type route struct {
		pattern string
		guard   guard
		handler http.Handler
	}
	routes := []route{
		{"GET /healthz", loopback, healthz(cfg.Version)},
		{"POST /mcp", bearer, mcp},
		{"GET /mcp", bearer, mcp},
		{"DELETE /mcp", bearer, mcp},
		{"GET /auth/{connector}", loopback, h.AuthConfirm},
		{"POST /auth/{connector}", guard{origin: true, sameOrigin: true}, h.AuthStart},
		{"/admin/", admin, h.Admin},
		{"GET /ui/", loopback, h.UI},
		{"POST /ui/", guard{origin: true, sameOrigin: true}, h.UI},
	}

	if cfg.CallbackPath != "" {
		routes = append(routes, route{"GET " + cfg.CallbackPath, guard{}, h.OAuthCallback})
	}

	gs := guards{
		port:     cfg.Port,
		ip:       cfg.IP.Unmap(),
		callback: cfg.CallbackPath,
		tokenSum: clienttoken.Digest(cfg.Token),
		clients:  cfg.Clients,
		log:      cfg.Logger,
	}
	mux := http.NewServeMux()

	for _, rt := range routes {
		mux.Handle(rt.pattern, gs.wrap(rt.guard, orNotImplemented(rt.handler)))
	}

	return gs.host(mux), nil
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
