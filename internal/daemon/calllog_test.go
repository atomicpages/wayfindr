package daemon_test

import (
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/drivers"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// The call log (docs/internal/plans/call-log.md): the daemon records each
// call_tool call off the request's path, reads calls.retention live, and
// prunes on its clock.

func (r running) calls(t *testing.T) admin.Calls {
	t.Helper()
	res, err := admin.NewClient("http://"+r.addr, "", r.client).Calls(t.Context(), admin.CallsQuery{Limit: admin.MaxCallsLimit})

	if err != nil {
		t.Fatalf("admin calls: %v", err)
	}

	return res
}

// waitCalls polls /admin/calls until it lists n calls, and returns them.
func (r running) waitCalls(t *testing.T, n int) admin.Calls {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)

	for {
		got := r.calls(t)

		if len(got.Calls) == n {
			return got
		}

		if time.Now().After(deadline) {
			t.Fatalf("call log never listed %d calls; last: %+v", n, got)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// storedCalls opens h's store, once the daemon has stopped, and lists its
// calls.
func storedCalls(t *testing.T, h host) []store.Call {
	t.Helper()
	st, err := drivers.Open(t.Context(), "sqlite", h.paths.DBFile())

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })
	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatal(err)
	}

	calls, err := st.ListCalls(t.Context(), store.CallFilter{UserID: user, Limit: 1000})

	if err != nil {
		t.Fatal(err)
	}

	return calls
}

func TestCallLogRecordsEveryCallAndWritesTheQueueAtShutdown(t *testing.T) {
	h := newHost(t)
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"files": stdioConnector(t, fakeupstream.V20251125)},
	})
	r.waitStatus(t, "files synced", synced("files"))

	call(t, r.mcp, "call_tool", map[string]any{"name": "files.echo"})
	got := r.waitCalls(t, 1)

	if got.Retention != "720h0m0s" {
		t.Errorf("retention = %q, want the 30-day default", got.Retention)
	}

	if c := got.Calls[0]; c.Connector != "files" || c.Tool != "echo" || c.Result != store.CallOK || c.DurationMS == nil {
		t.Errorf("call = %+v, want files.echo ok with a duration", c)
	}

	// Calls made right before shutdown are still written: shutdown stops
	// the writer only once it has stored what is queued.
	const burst = 20

	for range burst {
		call(t, r.mcp, "call_tool", map[string]any{"name": "files.echo"})
	}

	r.stop()

	if n := len(storedCalls(t, h)); n != 1+burst {
		t.Errorf("stored %d calls after shutdown, want %d", n, 1+burst)
	}
}

func TestCallLogOffRecordsNothingUntilAReloadTurnsItOn(t *testing.T) {
	h := newHost(t)
	listen := freeAddr(t)
	files := stdioConnector(t, fakeupstream.V20251125)
	r := serve(t, h, h.options(t), map[string]any{
		"server":     map[string]any{"listen": listen},
		"calls":      map[string]any{"retention": "0s"},
		"connectors": map[string]any{"files": files},
	})
	r.waitStatus(t, "files synced", synced("files"))

	call(t, r.mcp, "call_tool", map[string]any{"name": "files.echo"})

	// Record is a no-op while the log is off, so nothing can be queued.
	if got := r.calls(t); got.Retention != "0s" || len(got.Calls) != 0 {
		t.Fatalf("with retention 0s: %+v, want retention 0s and no calls", got)
	}

	h.writeConfig(t, map[string]any{
		"server":     map[string]any{"listen": listen},
		"calls":      map[string]any{"retention": "1h"},
		"connectors": map[string]any{"files": files},
	})
	deadline := time.Now().Add(15 * time.Second)

	for r.calls(t).Retention != "1h0m0s" {
		if time.Now().After(deadline) {
			t.Fatal("retention never became 1h after the reload")
		}

		time.Sleep(10 * time.Millisecond)
	}

	call(t, r.mcp, "call_tool", map[string]any{"name": "files.echo"})
	r.waitCalls(t, 1)
}

func TestCallLogPrunesAtStartupAndEveryHour(t *testing.T) {
	h := newHost(t)
	initHost(t, h, map[string]any{
		"server": map[string]any{"listen": freeAddr(t)},
		"calls":  map[string]any{"retention": "2h"},
	})

	// Three calls, older than the retention at startup, at the first
	// hourly prune, and at neither.
	st, err := drivers.Open(t.Context(), "sqlite", h.paths.DBFile())

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatal(err)
	}

	for _, ago := range []time.Duration{3 * time.Hour, 90 * time.Minute, 30 * time.Minute} {
		if err := st.RecordCall(t.Context(), store.Call{UserID: user, Connector: "files", Tool: "echo",
			StartedAt: epoch.Add(-ago), Result: store.CallOK}); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	clock := newFakeClock()
	opts := h.options(t)
	opts.Clock = clock
	r := start(t, h, opts)

	// The writer arms its hourly timer once the startup prune is done.
	clock.waitArmed(t, epoch.Add(time.Hour))

	if got := r.calls(t); len(got.Calls) != 2 {
		t.Fatalf("after the startup prune: %d calls, want 2 (the one from 3h ago gone)", len(got.Calls))
	}

	clock.advance(time.Hour)
	clock.waitArmed(t, epoch.Add(2*time.Hour))

	got := r.calls(t)

	if len(got.Calls) != 1 || !got.Calls[0].StartedAt.Equal(epoch.Add(-30*time.Minute)) {
		t.Errorf("after the hourly prune: %+v, want only the call from 30m ago", got.Calls)
	}
}
