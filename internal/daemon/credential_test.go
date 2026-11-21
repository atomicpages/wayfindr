package daemon_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// The tests in this file cover the daemon's recovery after a credential
// change, driven as the user does: an auth flow's callback stores a token.

// A credential change lists the connector's tools once: the recovery waits
// for the sync its new session requested and starts none of its own, and
// the flow's long-poll still reports the connector with its tools
// (wayfindr-zgp.12).
func TestCredentialChangeListsToolsOnce(t *testing.T) {
	// Registered first, so it runs last: after the daemon has stopped.
	t.Cleanup(func() { goleak.VerifyNone(t) })
	lists := countLists(t, fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL)
	as := fakeas.Start(t, fakeas.WithResourceHandler(reverseProxy(t, lists.URL)))
	h := newHost(t)
	r := serve(t, h, h.options(t), oauthConfig(freeAddr(t), as.ResourceURL))
	r.waitStatus(t, "linear needs_auth", func(s admin.Status) bool {
		c, ok := connector(s, "linear")

		return ok && c.State == upstream.StateNeedsAuth
	})
	before := lists.lists.Load()

	c := admin.NewClient("http://"+r.addr, "", r.client)
	f, err := c.StartAuth(t.Context(), "linear", admin.AuthStart{})

	if err != nil {
		t.Fatalf("StartAuth: %v", err)
	}

	_, back := visit(t, f.AuthorizeURL)

	if status, _ := visit(t, back); status != http.StatusOK {
		t.Fatalf("callback = %d, want 200", status)
	}

	got := waitFlow(t, c, f.FlowID)

	if got.State != admin.FlowDone || got.Result == nil || got.Result.State != upstream.StateReady || got.Result.Tools != 1 {
		t.Fatalf("WaitAuth = %+v (result %+v), want done with linear ready and its 1 tool", got, got.Result)
	}

	if n := lists.lists.Load() - before; n != 1 {
		t.Errorf("tools/list requests for the credential change = %d, want 1", n)
	}

	r.stop()
}

// The reconnect a credential change starts runs in the background, so the
// browser's callback never waits on it, and shutdown cancels it: an
// upstream that never answers the connect holds neither Serve nor a
// goroutine past it.
func TestShutdownCancelsACredentialReconnectThatHangs(t *testing.T) {
	// Registered first, so it runs last: after the upstream is released and
	// closed.
	t.Cleanup(func() { goleak.VerifyNone(t) })
	proxy := reverseProxy(t, fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL)
	// It forwards the authorized requests to the fake until hanging is set;
	// from then on it accepts every POST, the reconnect's initialize among
	// them, and never answers it.
	var hanging atomic.Bool
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	as := fakeas.Start(t, fakeas.WithResourceHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hanging.Load() || r.Method != http.MethodPost {
			proxy.ServeHTTP(w, r)

			return
		}

		select {
		case arrived <- struct{}{}:
		default:
		}

		select {
		case <-r.Context().Done():
		case <-release:
		}
	})))
	t.Cleanup(func() { close(release) })

	h := newHost(t)
	r := serve(t, h, h.options(t), oauthConfig(freeAddr(t), as.ResourceURL))
	r.waitStatus(t, "linear needs_auth", func(s admin.Status) bool {
		c, ok := connector(s, "linear")

		return ok && c.State == upstream.StateNeedsAuth
	})
	f, err := admin.NewClient("http://"+r.addr, "", r.client).StartAuth(t.Context(), "linear", admin.AuthStart{})

	if err != nil {
		t.Fatalf("StartAuth: %v", err)
	}

	_, back := visit(t, f.AuthorizeURL)

	hanging.Store(true)

	if status, _ := visit(t, back); status != http.StatusOK {
		t.Fatalf("callback = %d, want 200 without waiting on the reconnect", status)
	}

	select {
	case <-arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("the credential change never reconnected to the upstream")
	}

	// Well inside the 60s connect timeout, which alone would end it.
	const budget = 5 * time.Second
	stopped := make(chan struct{})
	go func() {
		r.stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(budget):
		t.Fatalf("Serve did not return within %v of shutdown while a credential reconnect hung", budget)
	}
}
