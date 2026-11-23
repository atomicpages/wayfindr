package oauthserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/httpclient"
	"github.com/atomicpages/wayfindr/internal/store"
)

// Options is what New needs. The daemon builds it once.
type Options struct {
	// Server returns the running config's [server] table, read on every
	// request.
	Server func() config.Server
	Grants Grants
	// ApproveURL returns the console's approve page for pending authorization
	// id, on the daemon's loopback address.
	ApproveURL func(id string) string
	// Client fetches CIMD documents. The daemon passes
	// httpclient.PublicOnly(); New uses a copy that follows no redirect.
	// Nil means httpclient.PublicOnly().
	Client *http.Client
	// FetchTimeout bounds one CIMD fetch, through its request's context;
	// zero means FetchTimeout. Tests shorten it.
	FetchTimeout time.Duration
	// Now is the clock; nil means time.Now.
	Now    func() time.Time
	Logger zerolog.Logger
}

// Request is a pending authorization (GLOSSARY) as the approve page
// shows it.
type Request struct {
	ID           string
	ClientID     string
	ClientName   string // printable, at most 100 bytes; the host when the document has none
	ClientHost   string // client_id's host[:port]
	RedirectHost string // redirect_uri's host[:port]
	StartedAt    time.Time
	// ReportedIP is the browser's address as the tunnel reported it (the
	// last X-Forwarded-For entry); "" when none. Shown, never trusted.
	ReportedIP string
	ExpiresAt  time.Time
	// Arrived is how many authorization requests became pending during
	// the arm this one was claimed from, this one included, counted when
	// it was claimed; zero until then. Only one was expected: above one,
	// Approve refuses with ErrContested.
	Arrived int
}

// pending is a pending authorization: what the approve page shows, and
// what the code it leads to is bound to.
type pending struct {
	req         Request
	redirectURI string
	state       string
	challenge   string
	resource    string
}

// code is an authorization code, kept by its SHA-256 until it is presented
// or expires.
type code struct {
	grant        string
	grantCreated time.Time
	clientID     string
	clientHost   string
	redirectURI  string
	challenge    string
	resource     string
	expires      time.Time
}

// spentCode is a code already presented, remembered so that presenting it
// again revokes its grant.
type spentCode struct {
	grant        string
	grantCreated time.Time
	clientHost   string
	until        time.Time
}

// Server is the authorization server: its four HTTP handlers, the pending
// authorizations, and the codes. Safe for concurrent use.
type Server struct {
	server     func() config.Server
	grants     Grants
	approveURL func(id string) string
	client     *http.Client
	// fetchTimeout bounds one CIMD fetch.
	fetchTimeout time.Duration
	now          func() time.Time
	log          zerolog.Logger
	page         *template.Template

	// fetches holds the CIMD fetches in flight, one per client ID. It is
	// outside mu, so a slow fetch holds up nothing else.
	fetches singleflight.Group

	mu sync.Mutex
	// armed is when the arm ends; zero when there is none.
	armed time.Time
	// arrived counts the authorization requests that became pending
	// during the current arm, evicted ones included. A new arm, one that
	// starts once the last has ended, starts it again at zero.
	arrived int
	// pending holds the unclaimed pending authorizations made while armed,
	// oldest first, at most MaxPending. Empty whenever the arm is not in
	// force.
	pending []*pending
	// claimed is the pending authorization the console claimed: the only
	// one Approve and Deny answer. Nil when there is none.
	claimed   *pending
	codes     map[[sha256.Size]byte]code
	spent     map[[sha256.Size]byte]spentCode
	documents map[string]cachedDocument
}

// New returns the authorization server for opts.
func New(opts Options) *Server {
	base := opts.Client
	if base == nil {
		base = httpclient.PublicOnly()
	}

	// The fetch's context bounds it (fetchTimeout), so the copy keeps the
	// base client's Timeout, which PublicOnly leaves unset.
	client := *base
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	fetchTimeout := opts.FetchTimeout
	if fetchTimeout <= 0 {
		fetchTimeout = FetchTimeout
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Server{
		server:       opts.Server,
		grants:       opts.Grants,
		approveURL:   opts.ApproveURL,
		client:       &client,
		fetchTimeout: fetchTimeout,
		now:          now,
		log:          opts.Logger,
		page:         template.Must(template.ParseFS(pages, "errorpage.html")),
		codes:        map[[sha256.Size]byte]code{},
		spent:        map[[sha256.Size]byte]spentCode{},
		documents:    map[string]cachedDocument{},
	}
}

// running returns the running [server] table and whether the server is
// on. Validation makes the issuer non-empty whenever it is enabled; an
// empty one counts as off all the same.
func (s *Server) running() (config.Server, bool) {
	cfg := s.server()

	return cfg, cfg.OAuth.Enabled && cfg.Issuer() != ""
}

// Arm lets authorization requests through for PendingTTL from now, and
// returns when the arm ends. There is one arm at a time: arming again
// resets the expiry, and keeps the pending authorizations and their count.
// While armed, every authorize request that passes every check through
// step 10 becomes a pending authorization (at most MaxPending; past it the
// oldest goes) and is counted (Request.Arrived). The arm ends when the
// console claims one of them (Claim) or when it expires, and then the
// unclaimed ones are dropped.
func (s *Server) Arm() (expires time.Time) {
	now := s.now()
	expires = now.Add(PendingTTL)

	s.mu.Lock()
	// Pending authorizations of an arm that has already ended go, so a
	// new arm starts with none, and counts from zero.
	s.sweepLocked(now)

	if !now.Before(s.armed) {
		s.arrived = 0
	}

	s.armed = expires
	s.mu.Unlock()

	s.log.Info().Time("expires_at", expires).Msg("oauth connection armed")

	return expires
}

// Disarm ends the arm and drops every pending authorization, the claimed
// request, and every authorization code not yet presented. The daemon
// calls it when a reload turns the server off, changes the resource
// (public_url), or changes the allowed clients, so nothing armed or
// claimed under the old settings is answered under the new ones: the user
// selects Connect a client again.
func (s *Server) Disarm() {
	s.mu.Lock()
	s.armed = time.Time{}
	s.arrived = 0
	s.pending = nil
	s.claimed = nil
	clear(s.codes)
	s.mu.Unlock()

	s.log.Info().Msg("oauth connection disarmed")
}

// Claim is the console's signed-in GET of the approve page for pending
// authorization id. While armed, claiming one of the pending
// authorizations consumes the arm, drops every other pending
// authorization, and returns the request, with Arrived set to how many
// became pending during the arm: from then on it is the only one
// Approve and Deny answer, and authorize says not_armed until the next
// Arm. Claiming the claimed request again (a reload) returns it with no
// other effect. False, with no effect, when id is not pending, the arm has
// ended, or the request is no longer current; and for any other id once
// one is claimed, unless the server was armed again and id is one of the
// new pending authorizations, which then replaces the claimed one.
func (s *Server) Claim(id string) (Request, bool) {
	cfg, on := s.running()
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if p := s.currentLocked(id, cfg, on, now); p != nil {
		return p.req, true
	}

	if !on || !now.Before(s.armed) {
		return Request{}, false
	}

	var found *pending

	// Every entry is compared, in constant time, so the time taken says
	// nothing about which one matched.
	for _, p := range s.pending {
		if subtle.ConstantTimeCompare([]byte(p.req.ID), []byte(id)) == 1 {
			found = p
		}
	}

	if found == nil || found.resource != Resource(cfg.Issuer()) {
		return Request{}, false
	}

	found.req.Arrived = s.arrived
	s.armed = time.Time{}
	s.pending = nil
	s.claimed = found

	s.log.Info().Str("client_host", found.req.ClientHost).Msg("oauth authorization claimed")

	return found.req, true
}

// Pending returns the claimed request if its id is id, with no effect:
// what Claim returns on a reload. False when id is unknown, not claimed
// (a request that is only pending reads false), expired, answered, or no
// longer current.
func (s *Server) Pending(id string) (Request, bool) {
	cfg, on := s.running()
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	p := s.currentLocked(id, cfg, on, now)
	if p == nil {
		return Request{}, false
	}

	return p.req, true
}

// Approve creates the grant name for connectors (Grants.CreateGrant, with
// the request's client ID and resource, CreatedAt now), makes a code bound
// to it (CodeTTL, single use), and returns redirect_uri with code, state,
// and iss. Only the claimed request is answered: ErrNoRequest when Pending
// would say false, which it does for a request that is pending but not
// claimed. A CreateGrant error (a taken name: store.ErrExists) is returned
// as is, and the request stays claimed so the user can pick another name.
// A request is answered at most once: it is taken out while the grant is
// stored, so Pending reads false meanwhile and a racing Approve or Deny
// gets ErrNoRequest. ErrNoRequest too when the request's client is no
// longer in server.oauth.clients.
//
// ErrContested, with no effect, when more than one authorization request
// became pending during the arm the request was claimed from
// (Request.Arrived): only the user's own was expected, so the one claimed
// may be a stranger's, opened in a tab the user didn't open. The request
// stays claimed, so Deny still answers it.
func (s *Server) Approve(ctx context.Context, id, name string, connectors []string) (redirect string, err error) {
	cfg, on := s.running()
	now := s.now()

	s.mu.Lock()

	p := s.currentLocked(id, cfg, on, now)
	if p == nil || !slices.Contains(cfg.OAuth.Clients, p.req.ClientID) {
		s.mu.Unlock()

		return "", ErrNoRequest
	}

	if p.req.Arrived > 1 {
		s.mu.Unlock()
		s.log.Info().Str("client_host", p.req.ClientHost).Int("arrived", p.req.Arrived).
			Msg("oauth approval refused: more than one request arrived")

		return "", ErrContested
	}
	// Taken out while the grant is stored, so a racing Approve or Deny
	// finds nothing.
	s.claimed = nil
	s.mu.Unlock()

	// The store keeps times to the millisecond; the code records the same
	// CreatedAt the store will return, so a replay can tell this grant from
	// one re-created under its name.
	created := now.Truncate(time.Millisecond)

	err = s.grants.CreateGrant(ctx, store.OAuthGrant{
		Name:       name,
		ClientID:   p.req.ClientID,
		Resource:   p.resource,
		Connectors: slices.Clone(connectors),
		CreatedAt:  created,
	})
	if err != nil {
		s.mu.Lock()
		// Back in place unless a newer claim replaced it meanwhile; it
		// keeps its expiry.
		if s.claimed == nil {
			s.claimed = p
		}

		s.mu.Unlock()

		return "", err
	}

	value := rand.Text()
	issued := s.now()

	s.mu.Lock()
	s.codes[sha256.Sum256([]byte(value))] = code{
		grant:        name,
		grantCreated: created,
		clientID:     p.req.ClientID,
		clientHost:   p.req.ClientHost,
		redirectURI:  p.redirectURI,
		challenge:    p.challenge,
		resource:     p.resource,
		expires:      issued.Add(CodeTTL),
	}
	s.mu.Unlock()

	s.log.Info().Str("grant", name).Str("client_host", p.req.ClientHost).Msg("oauth authorization approved")

	return withParams(p.redirectURI, url.Values{
		"code":  {value},
		"state": {p.state},
		"iss":   {cfg.Issuer()},
	}), nil
}

// Deny ends the claimed request and returns redirect_uri with
// error=access_denied, state, and iss. ErrNoRequest as for Approve.
func (s *Server) Deny(id string) (redirect string, err error) {
	cfg, on := s.running()
	now := s.now()

	s.mu.Lock()

	p := s.currentLocked(id, cfg, on, now)
	if p == nil {
		s.mu.Unlock()

		return "", ErrNoRequest
	}

	s.claimed = nil
	s.mu.Unlock()

	s.log.Info().Str("client_host", p.req.ClientHost).Msg("oauth authorization denied")

	return withParams(p.redirectURI, url.Values{
		"error": {"access_denied"},
		"state": {p.state},
		"iss":   {cfg.Issuer()},
	}), nil
}

// currentLocked returns the claimed request if its id is id and it is
// current: the server is on, it hasn't expired, and its resource is still
// the running one. Nil otherwise. The caller holds mu.
func (s *Server) currentLocked(id string, cfg config.Server, on bool, now time.Time) *pending {
	s.sweepLocked(now)

	p := s.claimed
	if p == nil || !on {
		return nil
	}

	if subtle.ConstantTimeCompare([]byte(p.req.ID), []byte(id)) != 1 {
		return nil
	}

	if p.resource != Resource(cfg.Issuer()) {
		return nil
	}

	return p
}

// sweepLocked drops what has expired: codes, spent codes, the claimed
// request, the pending authorizations (all of them once the arm has
// ended), and cached CIMD documents. The caller holds mu.
func (s *Server) sweepLocked(now time.Time) {
	for d, c := range s.codes {
		if !now.Before(c.expires) {
			delete(s.codes, d)
		}
	}

	for d, sc := range s.spent {
		if !now.Before(sc.until) {
			delete(s.spent, d)
		}
	}

	if s.claimed != nil && !now.Before(s.claimed.req.ExpiresAt) {
		s.claimed = nil
	}

	if !now.Before(s.armed) {
		s.pending = nil
	}

	s.pending = slices.DeleteFunc(s.pending, func(p *pending) bool {
		return !now.Before(p.req.ExpiresAt)
	})

	for id, e := range s.documents {
		if !now.Before(e.until) {
			delete(s.documents, id)
		}
	}
}

// withParams appends params to raw's own query (RFC 6749 §3.1.2), keeping
// raw byte for byte. raw has no fragment: authorize refused one.
func withParams(raw string, params url.Values) string {
	sep := "?"

	if i := strings.IndexByte(raw, '?'); i >= 0 {
		sep = "&"

		if i == len(raw)-1 || strings.HasSuffix(raw, "&") {
			sep = ""
		}
	}

	return raw + sep + params.Encode()
}
