package auth

import (
	"bytes"
	"cmp"
	"html/template"
	"net/http"
	"sync"
	"time"

	"github.com/atomicpages/wayfindr/internal/config"
)

// maxStartForm bounds the body of POST /auth/{connector}: one nonce.
const maxStartForm = 4 << 10

// maxNonces bounds the unspent nonces. Any web page can make the browser
// GET the confirmation page, so past the bound the oldest is dropped
// rather than memory growing.
const maxNonces = 1024

// page is what the browser pages show: a title, what happened, and what to
// do; Form is set on the confirmation page only, and Device on a device
// code flow's page only.
type page struct {
	Title   string
	Summary string
	Action  string
	Link    string
	Form    *confirmForm
	Device  *devicePrompt
}

// devicePrompt is a device code flow's page: the user code, and the link
// to the verification URI where it is entered.
type devicePrompt struct {
	UserCode string
	// URI is the verification URI to open: the complete one when the
	// server sent it.
	URI string
}

type confirmForm struct {
	Connector string
	Nonce     string
}

// pageTemplate is every browser page. html/template escapes the connector
// name and the advice.
const pageTemplate = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · Wayfindr</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem}
button{font:inherit;padding:.5rem 1.25rem}</style></head>
<body><h1>{{.Title}}</h1>
<p>{{.Summary}}</p>
{{with .Device}}<p>Enter this code:</p>
<p><code style="font-size:2rem;letter-spacing:.15em">{{.UserCode}}</code></p>
<p>at <a href="{{.URI}}" target="_blank" rel="noopener noreferrer">{{.URI}}</a></p>{{end}}
{{with .Form}}<form method="post">
<input type="hidden" name="nonce" value="{{.Nonce}}">
<button type="submit">Continue</button>
</form>{{end}}
{{with .Action}}<p>{{.}}</p>{{end}}
{{with .Link}}<p><a href="{{.}}">{{.}}</a></p>{{end}}
</body></html>
`

// renderPage writes p with status. The headers keep the page out of
// frames (the Continue button must not be clickjacked), out of caches, and
// keep the callback URL, which holds the code, out of any Referer. The
// confirmation page is same-origin instead: under no-referrer a browser
// posts its form with Origin: null, which httpd refuses (wayfindr-vngr).
func renderPage(w http.ResponseWriter, status int, p page) {
	var buf bytes.Buffer
	t := template.Must(template.New("page").Parse(pageTemplate))

	if err := t.Execute(&buf, p); err != nil {
		http.Error(w, "render page", http.StatusInternalServerError)

		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")

	if p.Form != nil {
		h.Set("Referrer-Policy", "same-origin")
	}

	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// renderAdvice renders err's advice with status under title; its link,
// when it has one, is the confirmation page that starts the flow again.
func renderAdvice(w http.ResponseWriter, status int, title string, err error) {
	p := page{Title: title, Summary: "The authorization failed."}

	if e, ok := AsError(err); ok {
		a := Advise(e)
		p.Summary, p.Action, p.Link = a.Summary, a.Action, a.Link
	}

	renderPage(w, status, p)
}

// Pages serves the browser's way into the auth flow (design §7.7): GET
// /auth/{connector} is a confirmation page with a Continue form, and its
// POST starts the flow the connector's grant names and redirects to the
// authorize URL, or, for a device code flow, shows the user code and a
// link to where it is entered. A plain GET that
// started a flow would let any web page push the user into a consent
// screen; httpd lets the POST through only with a same-origin Origin, and
// Pages also requires the form's nonce, which is single-use and expires
// with [FlowTTL]. Pages go through [Flows], as the admin API does, so a
// flow that cannot start is answered with the same status and advice.
type Pages struct {
	flows *Flows

	mu     sync.Mutex
	nonces map[string]pageNonce
}

type pageNonce struct {
	connector string
	expires   time.Time
}

// NewPages returns the pages of flows.
func NewPages(flows *Flows) *Pages {
	return &Pages{flows: flows, nonces: map[string]pageNonce{}}
}

// connector looks up the {connector} path value as an OAuth connector,
// rendering why not and returning false if it isn't one.
func (p *Pages) connector(w http.ResponseWriter, r *http.Request) (config.Connector, bool) {
	name := r.PathValue("connector")
	c, err := p.flows.connector(name)

	if err != nil {
		startFailed(w, name, err)

		return config.Connector{}, false
	}

	return c, true
}

// Confirm serves GET /auth/{connector}.
func (p *Pages) Confirm(w http.ResponseWriter, r *http.Request) {
	c, ok := p.connector(w, r)

	if !ok {
		return
	}

	nonce := random(32)
	now := p.flows.b.now()
	p.mu.Lock()
	oldest := ""

	for n, v := range p.nonces {
		if now.After(v.expires) {
			delete(p.nonces, n)
		} else if oldest == "" || v.expires.Before(p.nonces[oldest].expires) {
			oldest = n
		}
	}

	if len(p.nonces) >= maxNonces {
		delete(p.nonces, oldest)
	}

	p.nonces[nonce] = pageNonce{connector: c.Name, expires: now.Add(FlowTTL)}
	p.mu.Unlock()
	renderPage(w, http.StatusOK, page{
		Title:   "Authorize " + c.Name,
		Summary: "Continue to sign in with " + c.Name + "'s provider and allow Wayfindr to use it on your behalf.",
		Form:    &confirmForm{Connector: c.Name, Nonce: nonce},
	})
}

// Start serves POST /auth/{connector}. httpd has checked the Origin; Start
// checks and spends the form's nonce, then starts the flow.
func (p *Pages) Start(w http.ResponseWriter, r *http.Request) {
	c, ok := p.connector(w, r)

	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxStartForm)

	if err := r.ParseForm(); err != nil || !p.spend(r.PostForm.Get("nonce"), c.Name) {
		p.flows.b.log.Warn().Str("connector", c.Name).Msg("auth page post without a valid nonce")
		renderPage(w, http.StatusForbidden, page{
			Title:   "Authorization not started",
			Summary: "This form is stale or was not opened from Wayfindr.",
			Action:  "Open the authorization link again.",
			Link:    p.flows.b.Link(c.Name),
		})

		return
	}

	f, err := p.flows.Start(r.Context(), c.Name, StartOptions{})

	if err != nil {
		startFailed(w, c.Name, err)

		return
	}

	if f.Device() {
		renderPage(w, http.StatusOK, devicePage(f))

		return
	}

	// The authorize URL is the authorization server's, from its metadata
	// or the config, never from the request.
	http.Redirect(w, r, f.AuthorizeURL, http.StatusFound) //nolint:gosec // G710: see above
}

// devicePage is the page of a started device code flow: the user code and
// the link to the verification URI, which the flow has checked is https
// or loopback http. The poller completes the flow; the page does not wait.
func devicePage(f Flow) page {
	return page{
		Title:   "Authorize " + f.Connector,
		Summary: "Sign in with " + f.Connector + "'s provider on this or any other device, and enter the code there.",
		Action:  "Wayfindr finishes on its own once you have allowed access. You can then close this tab.",
		Device:  &devicePrompt{UserCode: f.UserCode, URI: cmp.Or(f.VerificationURIComplete, f.VerificationURI)},
	}
}

// spend reports whether nonce was issued for connector and is unexpired,
// and removes it either way.
func (p *Pages) spend(nonce, connector string) bool {
	if nonce == "" {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.nonces[nonce]
	delete(p.nonces, nonce)

	return ok && v.connector == connector && !p.flows.b.now().After(v.expires)
}
