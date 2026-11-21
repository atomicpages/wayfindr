package auth_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// flakyTokenEndpoint makes h's broker reach the token endpoint through a
// transport that, while the returned flag is set, fails every token
// request the way fail says.
func flakyTokenEndpoint(h *brokerHarness, fail func(*http.Request) (*http.Response, error)) *atomic.Bool {
	var failing atomic.Bool
	h.opts.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if failing.Load() && strings.HasSuffix(r.URL.Path, "/token") {
			return fail(r)
		}

		return http.DefaultTransport.RoundTrip(r)
	})}
	h.broker = auth.NewBroker(h.opts)

	return &failing
}

// connectionFailure fails a request in transport, as a timeout does.
func connectionFailure(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection timed out")
}

// badGateway answers a proxy's 502 page: a 5xx with no OAuth error code.
func badGateway(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": {"text/html"}},
		Body: io.NopCloser(strings.NewReader("<h1>Bad Gateway</h1>")), Request: r,
	}, nil
}

// TestDeviceFlowPollsLessOftenAfterATransientFailure is RFC 8628 §3.5: a
// poll that gets no answer about the grant doesn't end the flow; the
// client polls less often, and the user's approval still completes it.
func TestDeviceFlowPollsLessOftenAfterATransientFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(*http.Request) (*http.Response, error)
	}{
		{"connection failure", connectionFailure},
		{"5xx without an error code", badGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBrokerHarness(t, fakeas.WithDeviceCode())
			failing := flakyTokenEndpoint(h, tc.fail)
			g := gatePolls(h)
			t.Cleanup(h.broker.Close)
			f := h.startDevice(t)

			if d := g.next(t); d != 5*time.Second {
				t.Errorf("first interval = %s, want 5s", d)
			}

			failing.Store(true)
			g.poll()

			if d := g.next(t); d != 10*time.Second {
				t.Errorf("interval after a failed poll = %s, want it doubled to 10s", d)
			}

			failing.Store(false)
			h.as.ApproveDevice(f.UserCode)
			g.poll()

			if res := h.waitDone(t, f.ID); res.State != auth.FlowDone || res.Err != nil {
				t.Fatalf("flow = %s %v, want done", res.State, res.Err)
			}

			if diff := cmp.Diff([]string{"linear"}, h.changed.get()); diff != "" {
				t.Errorf("OnCredentialChanged (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDeviceFlowFailingUntilItsDeadlineExpires: polls that never get an
// answer back off until the device code's deadline, which ends the flow
// as flow_expired.
func TestDeviceFlowFailingUntilItsDeadlineExpires(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithDeviceCode(), fakeas.WithDeviceTiming(120, 0))
	failing := flakyTokenEndpoint(h, connectionFailure)
	failing.Store(true)
	g := gatePolls(h)
	t.Cleanup(h.broker.Close)
	f := h.startDevice(t)

	var got []time.Duration

	for range 3 {
		got = append(got, g.next(t))
		g.poll()
	}

	got = append(got, g.next(t))

	if diff := cmp.Diff([]time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second}, got); diff != "" {
		t.Errorf("intervals (-want +got):\n%s", diff)
	}

	h.clock.Advance(2 * time.Minute)
	g.poll()

	if res := h.waitDone(t, f.ID); res.State != auth.FlowExpired || caseOf(res.Err) != auth.CaseFlowExpired {
		t.Errorf("flow = %s %v, want expired flow_expired", res.State, res.Err)
	}
}

// TestDeviceFlowEndsOnAnOAuthError: an error the token endpoint names is
// an answer about the grant, not a failure to reach it, and ends the flow.
func TestDeviceFlowEndsOnAnOAuthError(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithDeviceCode())
	g := gatePolls(h)
	t.Cleanup(h.broker.Close)
	f := h.startDevice(t)

	g.next(t)
	h.as.FailTokenRequests("invalid_grant")
	g.poll()

	if res := h.waitDone(t, f.ID); res.State != auth.FlowFailed || caseOf(res.Err) != auth.CaseTokenRevoked {
		t.Errorf("flow = %s %v, want failed token_revoked", res.State, res.Err)
	}
}
