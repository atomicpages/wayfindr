package webui

import (
	"crypto/rand"
	"sync"
	"time"

	"github.com/atomicpages/wayfindr/internal/admin"
)

const (
	// LoginPath is the console's login page. A login code rides in its
	// fragment, so it is never in a request line or a log.
	LoginPath = "/ui/login"
	// LoginCodeTTL is how long a login code works if it is not used.
	LoginCodeTTL = 60 * time.Second
	// SessionIdle is how long a session lasts without a request.
	SessionIdle = 12 * time.Hour
	// PendingTTL is how long a command connector waits for its confirm.
	PendingTTL = 5 * time.Minute
	// maxCodes and maxSessions bound what a stream of logins can hold, and
	// maxPending a session's unconfirmed adds; past them the oldest goes.
	maxCodes    = 16
	maxSessions = 64
	maxPending  = 4
)

// Sessions holds the console's one-time login codes and its browser
// sessions (ADR 0016). They live in memory only: a daemon restart signs
// every browser out, and nothing about a session is ever stored.
type Sessions struct {
	now func() time.Time

	mu       sync.Mutex
	codes    map[string]time.Time
	sessions map[string]*session
}

// session is one signed-in browser.
type session struct {
	// csrf is the token every console form carries.
	csrf string
	seen time.Time
	// pending are command connectors waiting for their confirm, by token
	// (ADR 0017). They hold secret values, in memory only.
	pending map[string]pendingAdd
}

// pendingAdd is a command connector the console showed for review.
type pendingAdd struct {
	spec    admin.ConnectorSpec
	expires time.Time
}

// NewSessions returns an empty session store. now nil means time.Now.
func NewSessions(now func() time.Time) *Sessions {
	if now == nil {
		now = time.Now
	}

	return &Sessions{now: now, codes: map[string]time.Time{}, sessions: map[string]*session{}}
}

// IssueLogin makes a single-use login code and returns the path that
// redeems it, with the code in the fragment, and when the code expires.
func (s *Sessions) IssueLogin() (path string, expires time.Time) {
	code := rand.Text()

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)

	if len(s.codes) >= maxCodes {
		delete(s.codes, oldest(s.codes, func(t time.Time) time.Time { return t }))
	}

	expires = now.Add(LoginCodeTTL)
	s.codes[code] = expires

	return LoginPath + "#" + code, expires
}

// redeem spends code and, when it was issued and is unexpired, starts a
// session and returns its ID.
func (s *Sessions) redeem(code string) (id string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	expires, found := s.codes[code]
	delete(s.codes, code)

	if !found || !now.Before(expires) {
		return "", false
	}

	s.pruneLocked(now)

	if len(s.sessions) >= maxSessions {
		delete(s.sessions, oldest(s.sessions, func(ss *session) time.Time { return ss.seen }))
	}

	id = rand.Text()
	s.sessions[id] = &session{csrf: rand.Text(), seen: now}

	return id, true
}

// lookup returns the CSRF token of session id when it is live, and counts
// this as a use.
func (s *Sessions) lookup(id string) (csrf string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, found := s.sessions[id]

	if !found {
		return "", false
	}

	now := s.now()

	if now.Sub(ss.seen) >= SessionIdle {
		delete(s.sessions, id)

		return "", false
	}

	ss.seen = now

	return ss.csrf, true
}

// pend keeps spec on session id for its confirm and returns the token
// that confirms it, single-use and valid for PendingTTL.
func (s *Sessions) pend(id string, spec admin.ConnectorSpec) (token string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, found := s.sessions[id]

	if !found {
		return "", false
	}

	now := s.now()

	if ss.pending == nil {
		ss.pending = map[string]pendingAdd{}
	}

	for t, p := range ss.pending {
		if !now.Before(p.expires) {
			delete(ss.pending, t)
		}
	}

	if len(ss.pending) >= maxPending {
		delete(ss.pending, oldest(ss.pending, func(p pendingAdd) time.Time { return p.expires }))
	}

	token = rand.Text()
	ss.pending[token] = pendingAdd{spec: spec, expires: now.Add(PendingTTL)}

	return token, true
}

// take spends token on session id and returns its spec when it was issued
// to that session and is unexpired.
func (s *Sessions) take(id, token string) (admin.ConnectorSpec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, found := s.sessions[id]

	if !found {
		return admin.ConnectorSpec{}, false
	}

	p, found := ss.pending[token]
	delete(ss.pending, token)

	if !found || !s.now().Before(p.expires) {
		return admin.ConnectorSpec{}, false
	}

	return p.spec, true
}

// pruneLocked drops expired codes and idle sessions.
func (s *Sessions) pruneLocked(now time.Time) {
	for c, exp := range s.codes {
		if !now.Before(exp) {
			delete(s.codes, c)
		}
	}

	for id, ss := range s.sessions {
		if now.Sub(ss.seen) >= SessionIdle {
			delete(s.sessions, id)
		}
	}
}

// oldest is the key of m whose time is earliest.
func oldest[V any](m map[string]V, at func(V) time.Time) string {
	var (
		key   string
		first time.Time
	)

	for k, v := range m {
		if t := at(v); key == "" || t.Before(first) {
			key, first = k, t
		}
	}

	return key
}
