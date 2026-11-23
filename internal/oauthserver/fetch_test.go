package oauthserver_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/oauthserver"
)

// The CIMD fetch outside the server mutex (design §7.11, CIMD document): it
// is bounded by FetchTimeout, shared by every request for the same client,
// and not cancelled when the request that started it goes away.

// blockingDocument makes the CIMD server hold each request until release
// is closed (or the request goes away), then serve the valid document. It
// returns a channel that receives once per request the server takes.
func (h *harness) blockingDocument(release <-chan struct{}) <-chan struct{} {
	entered := make(chan struct{}, 64)
	serve := h.serveDocument(h.document())

	h.serve(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}

		select {
		case <-release:
			serve(w, r)
		case <-r.Context().Done():
		}
	})

	return entered
}

func TestCIMDFetchTimesOut(t *testing.T) {
	h := newHarness(t, func(o *oauthserver.Options) { o.FetchTimeout = 50 * time.Millisecond })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.blockingDocument(release)
	h.srv.Arm()

	if got := pageCase(h.authorize(h.query().Encode(), nil)); got != "client_document" {
		t.Errorf("case %q, want client_document", got)
	}

	if !strings.Contains(h.logs.String(), `"reason":"timeout"`) {
		t.Errorf("the failure is not logged as a timeout: %s", h.logs.String())
	}
}

func TestCIMDConcurrentRequestsShareOneFetch(t *testing.T) {
	const n = 8

	h := newHarness(t)
	release := make(chan struct{})
	entered := h.blockingDocument(release)
	h.srv.Arm()

	codes := make([]int, n)
	started := make(chan struct{}, n)

	var wg sync.WaitGroup

	for i := range n {
		wg.Go(func() {
			started <- struct{}{}

			codes[i] = h.authorize(h.query().Encode(), nil).Code
		})
	}

	<-entered

	for range n {
		<-started
	}

	close(release)
	wg.Wait()

	if got := h.fetches(); got != 1 {
		t.Errorf("%d fetches for %d concurrent requests, want 1", got, n)
	}

	found := 0

	for _, c := range codes {
		if c == http.StatusFound {
			found++
		}
	}

	// Authorize never consumes the arm: every one is kept pending.
	if found != n {
		t.Errorf("%d requests were let through (%v), want %d", found, codes, n)
	}
}

func TestCIMDFetchOutlivesTheRequestThatStartedIt(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	entered := h.blockingDocument(release)
	h.srv.Arm()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan string, 1)

	go func() { done <- pageCase(h.authorizeCtx(ctx, h.query().Encode(), nil)) }()

	<-entered
	cancel()

	if got := <-done; got != "client_document" {
		t.Errorf("the request that went away: case %q, want client_document", got)
	}

	close(release)

	// Had the fetch been cancelled with its request, its failure would be
	// cached and this would be client_document too.
	if w := h.authorize(h.query().Encode(), nil); w.Code != http.StatusFound {
		t.Errorf("next request: %d, case %q; want 302 from the shared fetch's document", w.Code, pageCase(w))
	}

	if got := h.fetches(); got != 1 {
		t.Errorf("%d fetches, want 1", got)
	}
}
