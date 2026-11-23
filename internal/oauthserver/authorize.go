package oauthserver

import (
	"bytes"
	"crypto/rand"
	"embed"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
)

//go:embed errorpage.html
var pages embed.FS

// maxState is the longest state authorize keeps, in bytes.
const maxState = 2 << 10

// pageCase is why authorize answered the error page (design §7.11): a
// closed set, carried in the page's data-case attribute.
type pageCase string

const (
	caseClientNotAllowed        pageCase = "client_not_allowed"
	caseNotNavigation           pageCase = "not_navigation"
	caseNotArmed                pageCase = "not_armed"
	caseDuplicateParameter      pageCase = "duplicate_parameter"
	caseUnsupportedResponseType pageCase = "unsupported_response_type"
	caseInvalidPKCE             pageCase = "invalid_pkce"
	caseInvalidResource         pageCase = "invalid_resource"
	caseMissingState            pageCase = "missing_state"
	caseInvalidRedirectURI      pageCase = "invalid_redirect_uri"
	caseClientDocument          pageCase = "client_document"
	caseRedirectNotRegistered   pageCase = "redirect_not_registered"
)

// sentence is the page's one fixed sentence for c. Nothing in it comes from
// the request.
func (c pageCase) sentence() string {
	switch c {
	case caseClientNotAllowed:
		return "This app isn't one Wayfindr is set up to let connect."
	case caseNotNavigation:
		return "This request didn't come from opening the page in a browser."
	case caseNotArmed:
		return "Wayfindr isn't expecting a connection. Open Wayfindr's console, choose Connect, then try again in the app."
	case caseDuplicateParameter:
		return "The app sent the same setting twice."
	case caseUnsupportedResponseType:
		return "The app asked for a kind of answer Wayfindr doesn't give."
	case caseInvalidPKCE:
		return "The app didn't send a valid S256 PKCE challenge."
	case caseInvalidResource:
		return "The app asked for an address other than this Wayfindr's /mcp."
	case caseMissingState:
		return "The app sent no state, or one that is too long."
	case caseInvalidRedirectURI:
		return "The app's return address isn't a valid https address."
	case caseClientDocument:
		return "Wayfindr couldn't read a valid metadata document for this app."
	case caseRedirectNotRegistered:
		return "The app's return address isn't listed in its metadata document."
	}

	return ""
}

// Authorize serves GET AuthorizePath: it checks an authorization request
// and, while armed, keeps it pending for the console's approve page
// (design §7.11). It never consumes the arm: the console's Claim does.
// Every refusal is the error page, never a redirect: the redirect_uri is
// not trusted until the client's document lists it. 404 while off.
func (s *Server) Authorize(w http.ResponseWriter, r *http.Request) {
	cfg, on := s.running()
	if !on {
		http.NotFound(w, r)

		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)

		return
	}

	location, c := s.authorize(r, cfg.Issuer(), cfg.OAuth.Clients)
	if c != "" {
		s.log.Debug().Str("case", string(c)).Msg("oauth authorize refused")
		s.writePage(w, c)

		return
	}

	h := w.Header()
	h.Set("Location", location)
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusFound)
}

// authorize runs authorize's steps in order and returns the approve page's
// URL, or the case of the first step that failed.
func (s *Server) authorize(r *http.Request, issuer string, clients []string) (string, pageCase) {
	// 1. The allowlist, before anything else: an unparsable query is not
	// trusted to say which client it is.
	query, err := url.ParseQuery(r.URL.RawQuery)
	ids := query["client_id"]

	if err != nil || len(ids) != 1 || !slices.Contains(clients, ids[0]) {
		return "", caseClientNotAllowed
	}

	clientID := ids[0]

	// 2. Only a top-level page the user opens: no subresource, frame,
	// fetch, or prefetch.
	if !isNavigation(r.Header) {
		return "", caseNotNavigation
	}

	// 3. Armed; read here, and again in step 11.
	if !s.isArmed() {
		return "", caseNotArmed
	}

	// 4. No parameter given twice.
	for _, v := range query {
		if len(v) != 1 {
			return "", caseDuplicateParameter
		}
	}

	// 5.
	if query.Get("response_type") != "code" {
		return "", caseUnsupportedResponseType
	}

	// 6.
	challenge := query.Get("code_challenge")
	if query.Get("code_challenge_method") != "S256" || !isChallenge(challenge) {
		return "", caseInvalidPKCE
	}

	// 7.
	resource := Resource(issuer)
	if query.Get("resource") != resource {
		return "", caseInvalidResource
	}

	// 8.
	state := query.Get("state")
	if state == "" || len(state) > maxState {
		return "", caseMissingState
	}

	// 9.
	redirectURI := query.Get("redirect_uri")

	redirect, ok := parseRedirect(redirectURI)
	if !ok {
		return "", caseInvalidRedirectURI
	}

	// 10. The only step that goes out: an allowed client's document,
	// cached.
	doc, err := s.document(r.Context(), clientID)
	if err != nil {
		return "", caseClientDocument
	}

	if !slices.Contains(doc.redirectURIs, redirectURI) {
		return "", caseRedirectNotRegistered
	}

	clientHost := hostOf(clientID)
	name := doc.name

	if name == "" {
		name = clientHost
	}

	id := rand.Text()
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	// 11. Still armed: a claim or the expiry may have ended the arm
	// meanwhile. The arm is not consumed here.
	if !now.Before(s.armed) {
		return "", caseNotArmed
	}

	// 12. Kept beside the others, the oldest dropped past MaxPending, and
	// counted: the count includes the dropped.
	s.arrived++

	if len(s.pending) >= MaxPending {
		s.pending = slices.Delete(s.pending, 0, len(s.pending)-MaxPending+1)
	}

	s.pending = append(s.pending, &pending{
		req: Request{
			ID:           id,
			ClientID:     clientID,
			ClientName:   name,
			ClientHost:   clientHost,
			RedirectHost: redirect.Host,
			StartedAt:    now,
			ReportedIP:   reportedIP(r),
			ExpiresAt:    now.Add(PendingTTL),
		},
		redirectURI: redirectURI,
		state:       state,
		challenge:   challenge,
		resource:    resource,
	})

	s.log.Debug().Str("client_host", clientHost).Msg("oauth authorization pending")

	return s.approveURL(id), ""
}

// isArmed reports whether an arm is in force now.
func (s *Server) isArmed() bool {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	return now.Before(s.armed)
}

// writePage answers the error page for c.
func (s *Server) writePage(w http.ResponseWriter, c pageCase) {
	var body bytes.Buffer

	err := s.page.Execute(&body, struct{ Case, Sentence string }{string(c), c.sentence()})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write(body.Bytes())
}

// isChallenge reports whether v is 43 base64url characters, the length of
// an S256 digest.
func isChallenge(v string) bool {
	if len(v) != 43 {
		return false
	}

	for i := range len(v) {
		c := v[i]
		if !isAlnum(c) && c != '-' && c != '_' {
			return false
		}
	}

	return true
}

// parseRedirect checks a redirect_uri: an absolute https URL with a host,
// or http on a loopback host, with no fragment.
func parseRedirect(raw string) (*url.URL, bool) {
	if raw == "" || strings.Contains(raw, "#") {
		return nil, false
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return nil, false
	}

	switch u.Scheme {
	case "https":
		return u, true
	case "http":
		switch u.Hostname() {
		case "127.0.0.1", "::1", "localhost":
			return u, true
		}
	}

	return nil, false
}

// hostOf returns raw's host[:port]; raw is an allowed client ID, which
// validation made an absolute URL.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	return u.Host
}

// isNavigation reports whether a request's headers allow it to be a
// top-level page the user opened. With any Sec-Fetch-* header it must be
// mode navigate and destination document, whatever its site, so neither a
// web page's no-cors request nor a hidden iframe passes. A prefetch or a
// prerender (Sec-Purpose, or the older Purpose) never passes. A request
// with none of these headers, from a client that isn't a browser, passes.
func isNavigation(h http.Header) bool {
	for _, name := range []string{"Sec-Purpose", "Purpose"} {
		for _, v := range h.Values(name) {
			v = strings.ToLower(v)
			if strings.Contains(v, "prefetch") || strings.Contains(v, "prerender") {
				return false
			}
		}
	}

	fetch := false

	for name := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "Sec-Fetch-") {
			fetch = true

			break
		}
	}

	if !fetch {
		return true
	}

	return h.Get("Sec-Fetch-Mode") == "navigate" && h.Get("Sec-Fetch-Dest") == "document"
}

// reportedIP is the browser's address as the tunnel reported it: the last
// X-Forwarded-For entry, the one the nearest proxy appended (the entries
// before it are whatever the browser sent), kept only if it is an IP
// address with no zone. Shown, never trusted.
//
// No vendor header counts, not even CF-Connecting-IP beside Cf-Ray: a
// browser can send both itself, and a tunnel that is not Cloudflare's
// passes them on.
func reportedIP(r *http.Request) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(lastForwardedFor(r.Header)))
	if err != nil || addr.Zone() != "" {
		return ""
	}

	return addr.String()
}

// lastForwardedFor is the last entry of the last X-Forwarded-For header;
// "" when there is none.
func lastForwardedFor(h http.Header) string {
	values := h.Values("X-Forwarded-For")
	if len(values) == 0 {
		return ""
	}

	last := values[len(values)-1]
	if i := strings.LastIndexByte(last, ','); i >= 0 {
		last = last[i+1:]
	}

	return last
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
