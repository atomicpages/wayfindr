package daemon_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/daemon"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// The tests in this file drive the daemon's poll of HTTP connectors (design
// §7.8 step 3) with a fake clock: the daemon arms a timer for its next poll,
// the test waits until that timer is armed, moves the clock past it, and
// waits until the timer for the poll after is armed, which the daemon does
// only once the polls it started have finished.

// epoch is where every fake clock starts.
var epoch = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

// fakeClock is a daemon.Clock whose time moves only when the test advances
// it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clock *fakeClock
	at    time.Time
	c     chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: epoch} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) daemon.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, at: c.now.Add(d), c: make(chan time.Time, 1)}

	if t.at.After(c.now) {
		c.timers = append(c.timers, t)
	} else {
		t.c <- c.now
	}

	return t
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	i := slices.Index(t.clock.timers, t)

	if i < 0 {
		return false
	}

	t.clock.timers = slices.Delete(t.clock.timers, i, i+1)

	return true
}

// advance moves the clock by d and fires every timer that is then due.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.timers = slices.DeleteFunc(c.timers, func(t *fakeTimer) bool {
		if t.at.After(c.now) {
			return false
		}

		t.c <- c.now

		return true
	})
}

// armed reports the deadlines of the timers not yet fired or stopped.
func (c *fakeClock) armed() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var at []time.Time

	for _, t := range c.timers {
		at = append(at, t.at)
	}

	return at
}

// waitArmed waits until a timer is armed for at: the daemon's next poll is
// due then.
func (c *fakeClock) waitArmed(t *testing.T, at time.Time) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)

	for !slices.ContainsFunc(c.armed(), at.Equal) {
		if time.Now().After(deadline) {
			t.Fatalf("no poll timer armed for %v (+%v); armed: %v", at, at.Sub(epoch), c.armed())
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// listCounter is a proxy in front of an upstream that counts the tools/list
// requests it forwards. Until up is set it answers every request with 503,
// as an upstream that is down does.
type listCounter struct {
	*httptest.Server
	lists atomic.Int64
	down  atomic.Bool
}

func countLists(t *testing.T, target string) *listCounter {
	t.Helper()
	u, err := url.Parse(target)

	if err != nil {
		t.Fatal(err)
	}

	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.FlushInterval = -1
	lc := &listCounter{}
	lc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lc.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)

			return
		}

		if r.Body != nil {
			body, err := io.ReadAll(r.Body)

			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)

				return
			}

			if bytes.Contains(body, []byte(`"method":"tools/list"`)) {
				lc.lists.Add(1)
			}

			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		lc.CloseClientConnections()
		lc.Close()
	})

	return lc
}

// envLookups is the daemon's ${env:…} lookup, which counts the lookups of
// each name: a connector whose headers name a variable looks it up on
// every connect, so the count tells how often the daemon tried to connect
// it.
type envLookups struct {
	mu     sync.Mutex
	values map[string]string
	counts map[string]int
}

func (e *envLookups) lookup(name string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.counts == nil {
		e.counts = map[string]int{}
	}

	e.counts[name]++
	v, ok := e.values[name]

	return v, ok
}

func (e *envLookups) count(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.counts[name]
}

// pollOptions are h's daemon options with clock and a jitter source that
// always draws 0, which puts every unhinted poll 10% early: 54m after the
// last one for a refresh_interval of 1h.
func pollOptions(t *testing.T, h host, clock *fakeClock) daemon.Options {
	opts := h.options(t)
	opts.Clock = clock
	opts.Jitter = func() float64 { return 0 }

	return opts
}

func stateIs(name string, want upstream.State) func(admin.Status) bool {
	return func(s admin.Status) bool {
		c, ok := connector(s, name)

		return ok && c.State == want
	}
}

func all(preds ...func(admin.Status) bool) func(admin.Status) bool {
	return func(s admin.Status) bool {
		for _, p := range preds {
			if !p(s) {
				return false
			}
		}

		return true
	}
}

func syncedOne(name string) func(admin.Status) bool {
	return func(s admin.Status) bool {
		c, ok := connector(s, name)

		return ok && c.State == upstream.StateReady && c.Tools > 0 && c.LastSync != nil
	}
}

func TestPollSyncsReadyHTTPConnectorsAndSkipsStdioAndThoseWaitingOnAuth(t *testing.T) {
	// Registered first, so it runs last: after the daemon has stopped.
	t.Cleanup(func() { goleak.VerifyNone(t) })
	issues := countLists(t, fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL)
	tracker := countLists(t, fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL)
	linear := countLists(t, fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL)
	env := &envLookups{values: map[string]string{"TRACKER_TEAM": "core"}}
	clock := newFakeClock()
	h := newHost(t)
	opts := pollOptions(t, h, clock)
	opts.LookupEnv = env.lookup
	r := serve(t, h, opts, map[string]any{
		"server": map[string]any{"listen": freeAddr(t)},
		"index":  map[string]any{"refresh_interval": "1h"},
		"connectors": map[string]any{
			"issues": map[string]any{"url": issues.URL},
			"files":  stdioConnector(t, fakeupstream.V20250618),
			// OAuth with no credential stored: needs_auth (a client_id,
			// since the fake publishes no OAuth metadata). Its header is
			// resolved on every connect, before the credential is read.
			"tracker": map[string]any{
				"url":     tracker.URL,
				"headers": map[string]string{"X-Team": "${env:TRACKER_TEAM}"},
				"auth":    map[string]any{"type": "oauth", "client_id": "tracker-app"},
			},
			// A variable that isn't set: auth_error.
			"linear": map[string]any{
				"url":     linear.URL,
				"headers": map[string]string{"Authorization": "Bearer ${env:LINEAR_TOKEN}"},
			},
		},
	})
	before := r.waitStatus(t, "issues and files synced, tracker needs_auth, linear auth_error", all(
		syncedOne("issues"), syncedOne("files"),
		stateIs("tracker", upstream.StateNeedsAuth), stateIs("linear", upstream.StateAuthError),
	))
	clock.waitArmed(t, epoch.Add(54*time.Minute))

	issuesLists := issues.lists.Load()
	trackerConnects, linearConnects := env.count("TRACKER_TEAM"), env.count("LINEAR_TOKEN")

	if trackerConnects == 0 || linearConnects == 0 {
		t.Fatalf("startup looked up TRACKER_TEAM %d and LINEAR_TOKEN %d times, want each at least once; is the count wired?", trackerConnects, linearConnects)
	}

	clock.advance(54 * time.Minute)
	clock.waitArmed(t, epoch.Add(108*time.Minute))

	if got := issues.lists.Load(); got != issuesLists+1 {
		t.Errorf("issues saw %d tools/list after the poll, want %d", got, issuesLists+1)
	}

	if got := env.count("TRACKER_TEAM"); got != trackerConnects {
		t.Errorf("the poll connected tracker (needs_auth): TRACKER_TEAM looked up %d times, want %d", got, trackerConnects)
	}

	if got := env.count("LINEAR_TOKEN"); got != linearConnects {
		t.Errorf("the poll connected linear (auth_error): LINEAR_TOKEN looked up %d times, want %d", got, linearConnects)
	}

	if n := tracker.lists.Load() + linear.lists.Load(); n != 0 {
		t.Errorf("tracker and linear saw %d tools/list, want none", n)
	}

	after := getStatus(t, r.client, "http://"+r.addr)
	was, _ := connector(before, "files")
	now, _ := connector(after, "files")

	if !now.LastSync.At.Equal(was.LastSync.At) {
		t.Errorf("files (stdio) last synced %v after the poll, want %v still", now.LastSync.At, was.LastSync.At)
	}

	r.stop()
}

// An unreachable HTTP connector is polled too, and the poll's reconnect
// brings it back once its upstream answers again.
func TestPollReconnectsAnUnreachableHTTPConnector(t *testing.T) {
	issues := countLists(t, fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL)
	issues.down.Store(true)
	clock := newFakeClock()
	h := newHost(t)
	r := serve(t, h, pollOptions(t, h, clock), map[string]any{
		"server":     map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"issues": map[string]any{"url": issues.URL}},
	})
	r.waitStatus(t, "issues unreachable", stateIs("issues", upstream.StateUnreachable))
	// No refresh_interval: the default, 1h.
	clock.waitArmed(t, epoch.Add(54*time.Minute))

	issues.down.Store(false)
	clock.advance(54 * time.Minute)
	clock.waitArmed(t, epoch.Add(108*time.Minute))

	got := getStatus(t, r.client, "http://"+r.addr)

	if !syncedOne("issues")(got) {
		t.Errorf("after the poll, issues = %+v, want ready and synced", got.Connectors)
	}
}

// A reload re-times the poll: a new refresh_interval applies from the
// reload, a connector it adds is polled on the new schedule, and one it
// removes is not polled again.
func TestPollFollowsAReload(t *testing.T) {
	issues := countLists(t, fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL)
	notes := countLists(t, fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL)
	clock := newFakeClock()
	h := newHost(t)
	listen := freeAddr(t)
	r := serve(t, h, pollOptions(t, h, clock), map[string]any{
		"server":     map[string]any{"listen": listen},
		"index":      map[string]any{"refresh_interval": "1h"},
		"connectors": map[string]any{"issues": map[string]any{"url": issues.URL}},
	})
	r.waitStatus(t, "issues synced", synced("issues"))
	clock.waitArmed(t, epoch.Add(54*time.Minute))

	// 20 minutes in, the interval drops to 10m, notes comes and issues goes.
	clock.advance(20 * time.Minute)
	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"index":      map[string]any{"refresh_interval": "10m"},
		"connectors": map[string]any{"notes": map[string]any{"url": notes.URL}},
	})
	r.waitStatus(t, "only notes synced", synced("notes"))
	// 10m less the 10% jitter, from the reload.
	clock.waitArmed(t, epoch.Add(29*time.Minute))
	issuesLists, notesLists := issues.lists.Load(), notes.lists.Load()

	clock.advance(9 * time.Minute)
	clock.waitArmed(t, epoch.Add(38*time.Minute))

	if got := notes.lists.Load(); got != notesLists+1 {
		t.Errorf("notes saw %d tools/list after the poll, want %d", got, notesLists+1)
	}

	if got := issues.lists.Load(); got != issuesLists {
		t.Errorf("issues (removed) saw %d tools/list after the poll, want %d", got, issuesLists)
	}
}

// A tools/list cache hint sets the connector's interval from the sync that
// read it on, and the jitter never polls before the hint runs out. The
// connect's sync counts: a new connector's first poll follows the hint it
// read, not refresh_interval (wayfindr-t08.6).
func TestPollFollowsTheToolsCacheHint(t *testing.T) {
	issues := fakeupstream.StartStreamable(t, fakeupstream.V20260728, fakeupstream.WithToolsTTL(5*time.Minute))
	clock := newFakeClock()
	h := newHost(t)
	r := serve(t, h, pollOptions(t, h, clock), map[string]any{
		"server":     map[string]any{"listen": freeAddr(t)},
		"index":      map[string]any{"refresh_interval": "1h"},
		"connectors": map[string]any{"issues": map[string]any{"url": issues.URL}},
	})
	r.waitStatus(t, "issues synced", synced("issues"))
	// The connect's sync read the 5m hint: the first poll is due within
	// [5m, 6m), at 5m for a jitter draw of 0, not at refresh_interval's 54m.
	clock.waitArmed(t, epoch.Add(5*time.Minute))

	clock.advance(5 * time.Minute)
	clock.waitArmed(t, epoch.Add(10*time.Minute))
	clock.advance(5 * time.Minute)
	clock.waitArmed(t, epoch.Add(15*time.Minute))
}

// Shutdown cancels a poll still waiting on its upstream: neither Serve nor
// a goroutine outlives it.
func TestShutdownCancelsAPollThatHangs(t *testing.T) {
	// Registered first, so it runs last: after the upstream is released and
	// closed.
	t.Cleanup(func() { goleak.VerifyNone(t) })
	fake := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	target, err := url.Parse(fake.URL)

	if err != nil {
		t.Fatal(err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	// It forwards to the fake until hanging is set; from then on it holds
	// every tools/list, the poll's among them, and never answers it.
	var hanging atomic.Bool
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)

		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))

		if !hanging.Load() || !bytes.Contains(body, []byte(`"method":"tools/list"`)) {
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
	}))
	t.Cleanup(front.Close)
	t.Cleanup(func() { close(release) })

	clock := newFakeClock()
	h := newHost(t)
	r := serve(t, h, pollOptions(t, h, clock), map[string]any{
		"server":     map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"slow": map[string]any{"url": front.URL}},
	})
	r.waitStatus(t, "slow synced", synced("slow"))
	clock.waitArmed(t, epoch.Add(54*time.Minute))

	hanging.Store(true)
	clock.advance(54 * time.Minute)

	select {
	case <-arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("the poll never listed the upstream's tools")
	}

	// Well inside the connector's 120s timeout, which alone would end it.
	const budget = 5 * time.Second
	stopped := make(chan struct{})
	go func() {
		r.stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(budget):
		t.Fatalf("Serve did not return within %v of shutdown while a poll hung", budget)
	}
}
