package auth_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// hookTokenEndpoint makes h's broker send every token request through hook;
// other requests go out unchanged.
func hookTokenEndpoint(h *brokerHarness, hook func(*http.Request) (*http.Response, error)) {
	h.opts.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			return hook(r)
		}

		return http.DefaultTransport.RoundTrip(r)
	})}
	h.broker = auth.NewBroker(h.opts)
}

// logLine returns the first line of logs whose message is msg.
func logLine(logs, msg string) string {
	for line := range strings.Lines(logs) {
		if strings.Contains(line, `"message":"`+msg+`"`) {
			return line
		}
	}

	return ""
}

// testRefreshTimeout is the refresh timeout the deadline tests set: long
// enough for a flight's discovery on a loaded machine under -race, short
// enough to wait out.
const testRefreshTimeout = 2 * time.Second

// hookFirstTokenRequest is hookTokenEndpoint for the first token request
// only; later ones go out unchanged.
func hookFirstTokenRequest(h *brokerHarness, hook func(*http.Request) (*http.Response, error)) {
	var used atomic.Bool

	hookTokenEndpoint(h, func(r *http.Request) (*http.Response, error) {
		if used.Swap(true) {
			return http.DefaultTransport.RoundTrip(r)
		}

		return hook(r)
	})
}

// bufferedAnswer sends r and returns the server's answer, its body read
// into memory.
func bufferedAnswer(r *http.Request) (*http.Response, []byte, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)

	if err != nil {
		return nil, nil, err
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	return resp, body, err
}

// readFunc is an io.Reader made of a function.
type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

// lateBody is a response body that hands out body only after wait, and,
// like net/http's, fails at once with ctx's cause if ctx, its request's
// context, ends first.
func lateBody(ctx context.Context, wait time.Duration, body []byte) io.ReadCloser {
	r := bytes.NewReader(body)
	var (
		once sync.Once
		err  error
	)

	return io.NopCloser(readFunc(func(p []byte) (int, error) {
		once.Do(func() {
			select {
			case <-ctx.Done():
				err = context.Cause(ctx)
			case <-time.After(wait):
			}
		})

		if err != nil {
			return 0, err
		}

		return r.Read(p)
	}))
}

// wantRotatedTokenStored refreshes the seeded grant once through h, then
// again once the new access token nears expiry: the second refresh spends
// the rotated refresh token, so it succeeds only if the first refresh
// stored it.
func wantRotatedTokenStored(t *testing.T, h *brokerHarness) {
	t.Helper()

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Fatalf("get = %d, %v; want 200", status, err)
	}

	h.clock.Advance(2 * time.Hour)

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Fatalf("get after second expiry = %d, %v; want 200", status, err)
	}

	if diff := cmp.Diff([]string{"", ""}, h.refreshes()); diff != "" {
		t.Errorf("refresh grants (-want +got):\n%s", diff)
	}
}

// A refresh whose answer arrives as its timeout runs out still stores the
// answer: the server has spent the old refresh token, and only the answer
// holds the new one (wayfindr-1j97).
func TestRefreshStoresAnAnswerThatArrivesAtTheDeadline(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithRotatingRefreshTokens())
	// The server answers at once; the answer is handed back only once the
	// refresh's deadline has ended its request, with a body that outlives it.
	hookFirstTokenRequest(h, func(r *http.Request) (*http.Response, error) {
		resp, body, err := bufferedAnswer(r)

		if err != nil {
			return nil, err
		}

		<-r.Context().Done()
		resp.Body = io.NopCloser(bytes.NewReader(body))

		return resp, nil
	})
	auth.SetRefreshTimeout(h.broker, testRefreshTimeout)
	h.seed(t, "", 30*time.Second)
	wantRotatedTokenStored(t, h)

	if line := logLine(h.logs.String(), "oauth token refreshed"); !strings.Contains(line, `"duration_ms":`) {
		t.Errorf("refreshed log line %q has no duration_ms", line)
	}
}

// A refresh whose answer has begun to arrive reads all of it even if its
// timeout runs out meanwhile: the deadline ends only the wait for the
// answer (wayfindr-1j97).
func TestRefreshReadsAnAnswerPastTheDeadline(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithRotatingRefreshTokens())
	// The headers come back at once; the body only after the refresh's
	// deadline has passed.
	hookFirstTokenRequest(h, func(r *http.Request) (*http.Response, error) {
		resp, body, err := bufferedAnswer(r)

		if err != nil {
			return nil, err
		}

		resp.Body = lateBody(r.Context(), testRefreshTimeout+500*time.Millisecond, body)

		return resp, nil
	})
	auth.SetRefreshTimeout(h.broker, testRefreshTimeout)
	h.seed(t, "", 30*time.Second)
	wantRotatedTokenStored(t, h)
}

// StopRefreshes waits for a running refresh, which outlives the request
// that started it, so its token is stored before the daemon closes the
// store; no refresh starts after it (wayfindr-1j97).
func TestStopRefreshesWaitsForARunningRefresh(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithRotatingRefreshTokens())
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once

	hookTokenEndpoint(h, func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(entered) })
		<-release

		return http.DefaultTransport.RoundTrip(r)
	})
	h.seed(t, "", 30*time.Second)

	type result struct {
		status int
		err    error
	}

	got := make(chan result, 1)

	go func() {
		status, err := h.get(t)
		got <- result{status, err}
	}()

	<-entered
	stopped := make(chan struct{})

	go func() {
		h.broker.StopRefreshes()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("StopRefreshes returned while a refresh was running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("StopRefreshes did not return once the refresh finished")
	}

	if r := <-got; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("get = %d, %v; want 200", r.status, r.err)
	}

	// The refreshed token was stored: the seeded one is inside the refresh
	// window, so only the stored one serves a call with refreshes stopped.
	requests := len(h.as.TokenRequests())

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Fatalf("get after StopRefreshes = %d, %v; want 200 with the stored token", status, err)
	}

	// Once that token nears expiry, no refresh starts.
	h.clock.Advance(2 * time.Hour)
	_, err := h.get(t)

	if e, ok := auth.AsError(err); !ok || e.Case != auth.CaseStore {
		t.Errorf("get after expiry = %v; want an auth error with case %s", err, auth.CaseStore)
	}

	if n := len(h.as.TokenRequests()); n != requests {
		t.Errorf("token requests = %d after StopRefreshes, want %d: no refresh may start", n, requests)
	}
}

// A refresh that StopRefreshes catches before its token request does not
// send it: the daemon is stopping, and a kill could land between the
// server rotating the refresh token and the answer being stored. Unspent,
// the refresh token serves the next start (wayfindr-1j97).
func TestStopRefreshesStopsARefreshBeforeItsTokenRequest(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithRotatingRefreshTokens())
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once

	// Discovery's first request waits for release.
	h.opts.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/token") {
			once.Do(func() {
				close(entered)
				<-release
			})
		}

		return http.DefaultTransport.RoundTrip(r)
	})}
	h.broker = auth.NewBroker(h.opts)
	h.seed(t, "", 30*time.Second)
	got := make(chan error, 1)

	go func() {
		_, err := h.get(t)
		got <- err
	}()

	<-entered
	stopped := make(chan struct{})

	go func() {
		h.broker.StopRefreshes()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("StopRefreshes returned while a refresh was running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("StopRefreshes did not return once the refresh's discovery finished")
	}

	if err := <-got; !isCase(err, auth.CaseStore) {
		t.Errorf("get = %v; want an auth error with case %s", err, auth.CaseStore)
	}

	if got := h.refreshes(); len(got) != 0 {
		t.Errorf("refresh grants = %q, want none: the refresh was stopped before its token request", got)
	}
}

// isCase reports whether err is an auth error with case c.
func isCase(err error, c auth.Case) bool {
	e, ok := auth.AsError(err)

	return ok && e.Case == c
}

// wantLostLine wants logs to log a lost refresh at error, with its
// duration.
func wantLostLine(t *testing.T, logs string) {
	t.Helper()
	line := logLine(logs, "oauth token refresh lost")

	if !strings.Contains(line, `"level":"error"`) || !strings.Contains(line, `"duration_ms":`) {
		t.Errorf("lost log line = %q, want it at error with duration_ms", line)
	}
}

// A success answer whose body cannot be read within bodyTimeout fails the
// refresh, which is logged as lost: the server may have rotated the
// refresh token, and only that body held the new one (wayfindr-1j97).
func TestRefreshLogsAnAnswerItCouldNotReadAsLost(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithRotatingRefreshTokens())
	// The headers come back at once; the body never does.
	hookFirstTokenRequest(h, func(r *http.Request) (*http.Response, error) {
		resp, body, err := bufferedAnswer(r)

		if err != nil {
			return nil, err
		}

		resp.Body = lateBody(r.Context(), time.Hour, body)

		return resp, nil
	})
	auth.SetBodyTimeout(h.broker, 200*time.Millisecond)
	h.seed(t, "", 30*time.Second)
	got := make(chan error, 1)

	go func() {
		_, err := h.get(t)
		got <- err
	}()

	select {
	case err := <-got:
		if !isCase(err, auth.CaseTransport) {
			t.Errorf("get = %v; want an auth error with case %s", err, auth.CaseTransport)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh did not give up on an answer that never ends")
	}

	wantLostLine(t, h.logs.String())
}

// failingPuts is a store whose PutCredential fails once fail is set.
type failingPuts struct {
	store.Store
	fail atomic.Bool
}

func (s *failingPuts) PutCredential(ctx context.Context, c store.Credential) error {
	if s.fail.Load() {
		return errors.New("disk full")
	}

	return s.Store.PutCredential(ctx, c)
}

// A refreshed token the store cannot keep is logged as lost: the server
// may have rotated the refresh token, and the answer that held the new one
// is gone (wayfindr-1j97).
func TestRefreshLogsATokenItCouldNotStoreAsLost(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithRotatingRefreshTokens())
	st := &failingPuts{Store: h.store}
	h.opts.Store = st
	h.broker = auth.NewBroker(h.opts)
	h.seed(t, "", 30*time.Second)
	st.fail.Store(true)

	if _, err := h.get(t); err == nil {
		t.Fatal("get succeeded with a store that cannot keep the refreshed token")
	}

	wantLostLine(t, h.logs.String())
}

// The token request has a budget of its own: discovery that spends most of
// the refresh's timeout does not leave the request too little time to be
// answered and read (wayfindr-1j97).
func TestRefreshGivesTheTokenRequestItsOwnBudget(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithRotatingRefreshTokens())
	var discovered atomic.Bool

	h.opts.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			// The server takes 2s of the 3s timeout to answer.
			select {
			case <-r.Context().Done():
				return nil, context.Cause(r.Context())
			case <-time.After(2 * time.Second):
			}
		case !discovered.Swap(true):
			// Discovery's first request takes 2s of it too.
			time.Sleep(2 * time.Second)
		}

		return http.DefaultTransport.RoundTrip(r)
	})}
	h.broker = auth.NewBroker(h.opts)
	auth.SetRefreshTimeout(h.broker, 3*time.Second)
	h.seed(t, "", 30*time.Second)

	if status, err := h.get(t); err != nil || status != http.StatusOK {
		t.Fatalf("get = %d, %v; want 200", status, err)
	}
}

// A revoke while a refresh is under way drops the refreshed token, as the
// user asked: it is logged as discarded, not as a lost refresh.
func TestRevokeDuringARefreshDiscardsItsToken(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithRotatingRefreshTokens())
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once

	hookTokenEndpoint(h, func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(entered) })
		<-release

		return http.DefaultTransport.RoundTrip(r)
	})
	h.seed(t, "", 30*time.Second)
	got := make(chan error, 1)

	go func() {
		_, err := h.get(t)
		got <- err
	}()

	<-entered

	if _, err := h.broker.Revoke(t.Context(), "linear"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	close(release)
	err := <-got

	if e, ok := auth.AsError(err); !ok || e.Case != auth.CaseNoCredential {
		t.Errorf("get = %v; want an auth error with case %s", err, auth.CaseNoCredential)
	}

	logs := h.logs.String()

	if line := logLine(logs, "oauth token refresh lost"); line != "" {
		t.Errorf("a revoke was logged as a lost refresh: %s", line)
	}

	if line := logLine(logs, "oauth token refresh discarded"); !strings.Contains(line, `"level":"info"`) {
		t.Errorf("discarded log line = %q, want it at info", line)
	}
}
