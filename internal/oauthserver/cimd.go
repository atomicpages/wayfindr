package oauthserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/atomicpages/wayfindr/internal/httpclient"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// maxClientName is the longest client name shown, in bytes.
const maxClientName = 100

// document is what authorize needs of a client's CIMD document.
type document struct {
	// name is the client_name, printable and clipped; "" when absent.
	name         string
	redirectURIs []string
}

// cachedDocument is a fetch's result, kept until until.
type cachedDocument struct {
	doc   document
	err   error
	until time.Time
}

// fetchError is why a CIMD fetch failed. reason is a closed set of short
// words for the log line; err, when set, is the cause.
type fetchError struct {
	reason string
	status int
	err    error
}

func (e *fetchError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("client metadata document: %s: %v", e.reason, e.err)
	}

	return "client metadata document: " + e.reason
}

func (e *fetchError) Unwrap() error { return e.err }

// document returns clientID's CIMD document from the cache, or fetches it.
// At most one fetch per client ID is in flight, outside mu; other callers
// wait for its result, or for their own ctx.
func (s *Server) document(ctx context.Context, clientID string) (document, error) {
	if e, ok := s.cached(clientID); ok {
		return e.doc, e.err
	}

	// The fetch is shared, so one caller going away must not cancel it for
	// the others.
	fetchCtx := context.WithoutCancel(ctx)

	ch := s.fetches.DoChan(clientID, func() (any, error) {
		// A fetch that ended just before this one started is in the cache.
		if e, ok := s.cached(clientID); ok {
			return e.doc, e.err
		}

		doc, err := s.fetch(fetchCtx, clientID)
		ttl := DocumentTTL

		if err != nil {
			ttl = FailureTTL
			s.logFetchFailure(clientID, err)
		}

		until := s.now().Add(ttl)

		s.mu.Lock()
		s.documents[clientID] = cachedDocument{doc: doc, err: err, until: until}
		s.mu.Unlock()

		return doc, err
	})

	select {
	case res := <-ch:
		doc, _ := res.Val.(document)

		return doc, res.Err
	case <-ctx.Done():
		return document{}, ctx.Err()
	}
}

// cached returns clientID's cache entry while it is fresh.
func (s *Server) cached(clientID string) (cachedDocument, bool) {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.documents[clientID]
	if !ok || !now.Before(e.until) {
		return cachedDocument{}, false
	}

	return e, true
}

// fetch GETs clientID, an allowed client's CIMD URL, through the client
// that follows no redirect, and checks the document.
func (s *Server) fetch(ctx context.Context, clientID string) (document, error) {
	u, err := url.Parse(clientID)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return document{}, &fetchError{reason: "not_https"}
	}

	ctx, cancel := context.WithTimeout(ctx, s.fetchTimeout)
	defer cancel()

	// G704: clientID is an allowed client (server.oauth.clients), and the
	// client dials only public addresses and follows no redirect.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil) //nolint:gosec // G704: see above
	if err != nil {
		return document{}, &fetchError{reason: "request", err: err}
	}

	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req) //nolint:gosec // G704: see the request above
	if err != nil {
		return document{}, &fetchError{reason: transportReason(err), err: err}
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return document{}, &fetchError{reason: "status", status: resp.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxDocument+1))
	if err != nil {
		return document{}, &fetchError{reason: transportReason(err), err: err}
	}

	if len(body) > MaxDocument {
		return document{}, &fetchError{reason: "size"}
	}

	return parseDocument(clientID, body)
}

// transportReason names a failed request or read for the log line.
func transportReason(err error) string {
	if errors.Is(err, httpclient.ErrNotPublic) {
		return "not_public"
	}

	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "timeout"
	}

	return "request"
}

// parseDocument checks a CIMD document fetched from clientID: one JSON
// object whose client_id is clientID, with a non-empty redirect_uris, the
// auth method none among those it names (both the singular field and
// SEP-3149's plural one), and a string client_name if any.
func parseDocument(clientID string, body []byte) (document, error) {
	var raw struct {
		ClientID     *string   `json:"client_id"`
		RedirectURIs []string  `json:"redirect_uris"`
		AuthMethod   *string   `json:"token_endpoint_auth_method"`
		AuthMethods  *[]string `json:"token_endpoint_auth_methods_supported"`
		ClientName   *string   `json:"client_name"`
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&raw); err != nil {
		return document{}, &fetchError{reason: "invalid_json"}
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return document{}, &fetchError{reason: "invalid_json"}
	}

	if raw.ClientID == nil || *raw.ClientID != clientID {
		return document{}, &fetchError{reason: "client_id_mismatch"}
	}

	if len(raw.RedirectURIs) == 0 {
		return document{}, &fetchError{reason: "no_redirect_uris"}
	}

	if raw.AuthMethod != nil || raw.AuthMethods != nil {
		none := raw.AuthMethod != nil && *raw.AuthMethod == "none"
		if raw.AuthMethods != nil && slices.Contains(*raw.AuthMethods, "none") {
			none = true
		}

		if !none {
			return document{}, &fetchError{reason: "auth_method"}
		}
	}

	var name string
	if raw.ClientName != nil {
		name = clip(strings.TrimSpace(redact.Printable(*raw.ClientName)), maxClientName)
	}

	return document{name: name, redirectURIs: raw.RedirectURIs}, nil
}

// clip cuts s to at most n bytes, at a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}

// logFetchFailure logs a failed fetch at warn: the client host and the
// reason, never the body.
func (s *Server) logFetchFailure(clientID string, err error) {
	ev := s.log.Warn().Str("client_host", hostOf(clientID))

	var fe *fetchError
	if errors.As(err, &fe) {
		ev = ev.Str("reason", fe.reason)

		if fe.status != 0 {
			ev = ev.Int("status", fe.status)
		}

		if fe.err != nil {
			ev = ev.Str("error", redact.URLQueries(fe.err.Error()))
		}
	}

	ev.Msg("oauth client metadata document refused")
}
