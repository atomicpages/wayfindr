// Package webui serves the daemon's local web console under /ui/ (design
// §7.10, ADR 0016): connectors, the call log, a search preview, and client
// tokens, as server-rendered pages, and a form that adds connectors
// (ADR 0017). It reads and acts only through the admin API's Client, so the
// browser and the CLI see the same answers; the daemon writes the config
// for an add, and the console never edits or removes a connector.
//
// httpd mounts New's handler at /ui/ behind its loopback Host and Origin
// guards (a POST only from the same origin); webui checks its own session
// cookie and each form's CSRF token. The pages use htmx for partial
// updates and work without it.
package webui

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

// Admin is the part of the admin API the console calls. *admin.Client
// implements it; the daemon passes admin.NewLocalClient, which calls the
// admin handler in process.
type Admin interface {
	Status(ctx context.Context) (admin.Status, error)
	StartAuth(ctx context.Context, connector string, start admin.AuthStart) (admin.AuthFlow, error)
	Revoke(ctx context.Context, connector string) (admin.Revoked, error)
	Calls(ctx context.Context, q admin.CallsQuery) (admin.Calls, error)
	Search(ctx context.Context, req admin.SearchRequest) (admin.SearchResult, error)
	Tools(ctx context.Context, connector string) (admin.ToolList, error)
	ListTokens(ctx context.Context) (admin.TokenList, error)
	CreateToken(ctx context.Context, name string, connectors []string) (admin.TokenCreated, error)
	RevokeToken(ctx context.Context, name string) (admin.TokenRevoked, error)
	AddConnector(ctx context.Context, spec admin.ConnectorSpec) (admin.ConnectorAdded, error)
}

// Options is what New needs. The daemon builds it once.
type Options struct {
	Admin    Admin
	Sessions *Sessions
	// Now is the clock pages read times against; nil means time.Now.
	Now    func() time.Time
	Logger zerolog.Logger
}

const (
	// cookieName is the session cookie. It is scoped to /ui, HttpOnly, and
	// SameSite=Strict.
	cookieName = "wayfindr_ui"
	// maxForm bounds a console form's body.
	maxForm = 16 << 10
	// csp allows only the console's own files, plus connector logos from
	// theSVG on jsDelivr (logos.go): no inline script or style, no frames.
	// form-action is left out on purpose: the sign-in form's answer
	// redirects to the authorization server, which Chrome checks against
	// form-action.
	csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' https://cdn.jsdelivr.net; " +
		"font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'"
)

// console holds the handlers' shared state.
type console struct {
	Options

	pages  map[string]*template.Template
	brands brandIndex
}

// New returns the handler for every /ui/ path.
func New(opts Options) (http.Handler, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	brands, err := loadBrands()

	if err != nil {
		return nil, err
	}

	pages, err := parsePages(brands)

	if err != nil {
		return nil, err
	}

	static, err := fs.Sub(staticFiles, "static")

	if err != nil {
		return nil, err
	}

	c := &console{Options: opts, pages: pages, brands: brands}
	mux := http.NewServeMux()
	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", staticHandler(static)))
	mux.HandleFunc("GET "+LoginPath, c.loginPage)
	mux.HandleFunc("POST "+LoginPath, c.login)
	mux.HandleFunc("GET /ui/{$}", c.authed(c.connectors))
	mux.HandleFunc("GET /ui/connectors/{name}", c.authed(c.connector))
	mux.HandleFunc("POST /ui/connectors/{name}/sign-in", c.authedForm(c.signIn))
	mux.HandleFunc("POST /ui/connectors/{name}/sign-out", c.authedForm(c.signOut))
	mux.HandleFunc("GET /ui/add", c.authed(c.addPage))
	mux.HandleFunc("POST /ui/add", c.authedForm(c.add))
	mux.HandleFunc("POST /ui/add/confirm", c.authedForm(c.confirmAdd))
	mux.HandleFunc("POST /ui/add/back", c.authedForm(c.backToAdd))
	mux.HandleFunc("GET /ui/calls", c.authed(c.calls))
	mux.HandleFunc("GET /ui/search", c.authed(c.search))
	mux.HandleFunc("GET /ui/tokens", c.authed(c.tokens))
	mux.HandleFunc("POST /ui/tokens", c.authedForm(c.createToken))
	mux.HandleFunc("POST /ui/tokens/{name}/revoke", c.authedForm(c.revokeToken))
	mux.HandleFunc("/ui/", c.notFound)

	return securityHeaders(mux), nil
}

// parsePages parses each page with the shared layout into its own set, so
// every page can define "title" and "content".
func parsePages(brands brandIndex) (map[string]*template.Template, error) {
	funcs := template.FuncMap{"code": codeSpans, "icon": icon, "logo": brands.logo}
	layout, err := template.New("layout").Funcs(funcs).ParseFS(templateFiles, "templates/layout.html")

	if err != nil {
		return nil, err
	}

	pages := map[string]*template.Template{}

	for _, name := range []string{"connectors", "connector", "calls", "search", "tokens", "error", "add", "review", "device"} {
		t, err := template.Must(layout.Clone()).ParseFS(templateFiles, "templates/"+name+".html")

		if err != nil {
			return nil, err
		}

		pages[name] = t
	}

	login, err := template.New("login").Funcs(funcs).ParseFS(templateFiles, "templates/login.html")

	if err != nil {
		return nil, err
	}

	pages["login"] = login

	return pages, nil
}

// securityHeaders sets the headers every console answer carries.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		// same-origin, not no-referrer: under no-referrer a browser sends
		// "Origin: null" on a form POST, which httpd's same-origin check
		// refuses. Nothing is sent to another origin either way.
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// staticHandler serves the embedded assets. They change only with the
// binary, so the browser revalidates rather than caches them.
func staticHandler(files fs.FS) http.Handler {
	fileServer := http.FileServerFS(files)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")

		if ct := staticType(path.Ext(r.URL.Path)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}

		fileServer.ServeHTTP(w, r)
	})
}

// staticType is the Content-Type of an embedded asset by its extension, or
// "" to let the file server decide. It is fixed here because nosniff needs
// the right type and the mime package can't be relied on for it: its table
// has no font types, and on Windows it takes types from the registry, which
// can say application/javascript, or even text/plain, for .js.
func staticType(ext string) string {
	switch ext {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".woff2":
		return "font/woff2"
	case ".txt":
		return "text/plain; charset=utf-8"
	}

	return ""
}

// sess is the signed-in browser a handler serves.
type sess struct{ id, csrf string }

// authed wraps a page handler: without a live session the browser goes to
// the login page.
func (c *console) authed(h func(http.ResponseWriter, *http.Request, sess)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := c.session(r)

		if !ok {
			toLogin(w, r)

			return
		}

		w.Header().Set("Cache-Control", "no-store")
		h(w, r, s)
	}
}

// authedForm wraps a form handler: it also needs the session's CSRF token
// in the form, on top of httpd's same-origin check.
func (c *console) authedForm(h func(http.ResponseWriter, *http.Request, sess)) http.HandlerFunc {
	return c.authed(func(w http.ResponseWriter, r *http.Request, s sess) {
		r.Body = http.MaxBytesReader(w, r.Body, maxForm)

		if err := r.ParseForm(); err != nil {
			http.Error(w, "malformed form", http.StatusBadRequest)

			return
		}

		if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(s.csrf)) != 1 {
			c.Logger.Warn().Str("path", r.URL.Path).Msg("console form without its csrf token")
			http.Error(w, "This form is out of date. Reload the page and try again.", http.StatusForbidden)

			return
		}

		h(w, r, s)
	})
}

// session reads the request's session cookie.
func (c *console) session(r *http.Request) (sess, bool) {
	ck, err := r.Cookie(cookieName)

	if err != nil {
		return sess{}, false
	}

	csrf, ok := c.Sessions.lookup(ck.Value)

	return sess{id: ck.Value, csrf: csrf}, ok
}

// toLogin sends the browser to the login page; an htmx request is told to
// navigate rather than swap the login page in.
func toLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", LoginPath)
		w.WriteHeader(http.StatusNoContent)

		return
	}

	http.Redirect(w, r, LoginPath, http.StatusSeeOther)
}

// loginPage serves GET /ui/login. Its script posts the code from the
// fragment; without one it says how to get one.
func (c *console) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := c.session(r); ok {
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)

		return
	}

	w.Header().Set("Cache-Control", "no-store")
	c.render(w, http.StatusOK, "login", loginView{})
}

// loginView is the login page's data.
type loginView struct{ Failed bool }

// login serves POST /ui/login: it spends the code and starts a session.
func (c *console) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)

	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)

		return
	}

	id, ok := c.Sessions.redeem(r.PostForm.Get("code"))

	if !ok {
		w.Header().Set("Cache-Control", "no-store")
		c.render(w, http.StatusUnauthorized, "login", loginView{Failed: true})

		return
	}

	// No Secure: the console is plain http on loopback, where some
	// browsers drop a Secure cookie. HttpOnly and SameSite=Strict hold.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: see above
		Name:     cookieName,
		Value:    id,
		Path:     "/ui",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

// notFound answers a /ui/ path the console doesn't serve.
func (c *console) notFound(w http.ResponseWriter, r *http.Request) {
	s, ok := c.session(r)

	if !ok {
		toLogin(w, r)

		return
	}

	c.fail(w, r, s, http.StatusNotFound, "There is no console page at "+r.URL.Path+".")
}

// render executes page into a buffer, so a template error never sends half
// a page, and writes it with status.
func (c *console) render(w http.ResponseWriter, status int, page string, data any) {
	var b bytes.Buffer

	t := c.pages[page]
	name := "layout"

	if page == "login" {
		name = "login"
	}

	if err := t.ExecuteTemplate(&b, name, data); err != nil {
		c.Logger.Error().Err(err).Str("page", page).Msg("render console page")
		http.Error(w, "The page could not be shown; see the daemon log.", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	// The status line is sent; a failed write has nobody to tell.
	_, _ = w.Write(b.Bytes())
}

// message is what the user reads for err: the daemon's own sentence for an
// admin API refusal, otherwise a pointer to the log, where err goes.
func (c *console) message(r *http.Request, err error) (int, string) {
	if ae := (*admin.APIError)(nil); errors.As(err, &ae) {
		return ae.Status, ae.Error()
	}

	c.Logger.Error().Err(err).Str("path", r.URL.Path).Msg("console request")

	return http.StatusInternalServerError, "The daemon could not answer; see the daemon log."
}

// icon renders the layout's sprite symbol i-<name>.
func icon(name string) template.HTML {
	return template.HTML(`<svg class="i" aria-hidden="true"><use href="#i-` + template.HTMLEscapeString(name) + `"/></svg>`) //nolint:gosec // G203: name is escaped
}

// codeSpans renders s with each `quoted` span as <code>. Every part is
// escaped, so s may be any text the daemon wrote.
func codeSpans(s string) template.HTML {
	parts := strings.Split(s, "`")
	var b strings.Builder

	for i, p := range parts {
		switch {
		case i%2 == 1 && i < len(parts)-1:
			b.WriteString("<code>")
			b.WriteString(template.HTMLEscapeString(p))
			b.WriteString("</code>")
		case i%2 == 1:
			// An unmatched backtick stays as text.
			b.WriteString("`")
			b.WriteString(template.HTMLEscapeString(p))
		default:
			b.WriteString(template.HTMLEscapeString(p))
		}
	}

	return template.HTML(b.String()) //nolint:gosec // G203: every part is escaped above
}
