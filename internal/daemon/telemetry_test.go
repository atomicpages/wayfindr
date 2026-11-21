package daemon_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/telemetry"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// reports is a fake collector that keeps the usage reports it receives.
type reports struct {
	mu   sync.Mutex
	got  []telemetry.Report
	errs []error
}

func collect(t *testing.T) (*reports, *httptest.Server) {
	t.Helper()
	rs := &reports{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep telemetry.Report
		body, err := io.ReadAll(r.Body)

		if err == nil {
			err = json.Unmarshal(body, &rep)
		}

		rs.mu.Lock()
		defer rs.mu.Unlock()

		if err != nil {
			rs.errs = append(rs.errs, err)
		}

		rs.got = append(rs.got, rep)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	return rs, srv
}

func (rs *reports) list() []telemetry.Report {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	return append([]telemetry.Report(nil), rs.got...)
}

// wait waits until the collector has n reports.
func (rs *reports) wait(t *testing.T, n int) []telemetry.Report {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)

	for len(rs.list()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("collector received %d reports, want %d", len(rs.list()), n)
		}

		time.Sleep(10 * time.Millisecond)
	}

	return rs.list()
}

// TestUsageReportCountsWithTheCallLogOffAndStopsWhenTurnedOff (ADR 0020): a
// release build with telemetry on sends its first report on the clock,
// with calls counted although the call log is off, and a reload that turns
// telemetry off sends nothing more.
func TestUsageReportCountsWithTheCallLogOffAndStopsWhenTurnedOff(t *testing.T) {
	rs, srv := collect(t)
	h := newHost(t)
	getenv := h.env.Getenv
	h.env.Getenv = func(k string) string {
		if k == telemetry.EndpointEnv {
			return srv.URL
		}

		return getenv(k)
	}
	clock := newFakeClock()
	opts := h.options(t)
	opts.Version, opts.Commit = "v0.4.0", "2e3a922"
	opts.Clock = clock
	opts.Jitter = func() float64 { return 0 }
	opts.ReportDifficulty = 1
	listen := freeAddr(t)
	files := stdioConnector(t, fakeupstream.V20251125)
	cfg := func(enabled bool, retention string) map[string]any {
		return map[string]any{
			"server":     map[string]any{"listen": listen},
			"calls":      map[string]any{"retention": retention},
			"telemetry":  map[string]any{"enabled": enabled},
			"connectors": map[string]any{"files": files},
		}
	}
	r := serve(t, h, opts, cfg(true, "0s"))
	r.waitStatus(t, "files synced", synced("files"))

	call(t, r.mcp, "call_tool", map[string]any{"name": "files.echo"})
	call(t, r.mcp, "call_tool", map[string]any{"name": "files.echo"})
	call(t, r.mcp, "search_tools", map[string]any{"query": "echo"})

	// With no jitter, the first report is due 5 minutes after startup.
	first := epoch.Add(5 * time.Minute)
	clock.waitArmed(t, first)
	clock.advance(5 * time.Minute)
	got := rs.wait(t, 1)[0]

	type summary struct {
		Version, Store       string
		Stdio, None          int
		Searches, OK         int64
		ClientsTotal         int64
		InstallIDFileWritten bool
	}

	var clients int64

	for _, n := range got.Clients {
		clients += n
	}

	_, statErr := os.Stat(h.paths.TelemetryIDFile())
	want := summary{Version: "v0.4.0", Store: "sqlite", Stdio: 1, None: 1, Searches: 1, OK: 2, ClientsTotal: 2, InstallIDFileWritten: true}
	gotSummary := summary{
		Version: got.Version, Store: got.Store,
		Stdio: got.Connectors.Transport.Stdio, None: got.Connectors.Auth.None,
		Searches: got.Searches, OK: got.Calls.OK, ClientsTotal: clients,
		InstallIDFileWritten: statErr == nil,
	}

	if diff := cmp.Diff(want, gotSummary); diff != "" {
		t.Errorf("first report (-want +got):\n%s", diff)
	}

	if !telemetry.Verify(got.InstallID, got.PoW.Date, got.PoW.Nonce, 1) {
		t.Errorf("first report's proof-of-work %+v does not verify", got.PoW)
	}

	// Turn telemetry off. The retention change marks when the reload has
	// applied: Reload applies telemetry first.
	h.writeConfig(t, cfg(false, "1h"))
	deadline := time.Now().Add(15 * time.Second)

	for r.calls(t).Retention != "1h0m0s" {
		if time.Now().After(deadline) {
			t.Fatal("the reload never applied")
		}

		time.Sleep(10 * time.Millisecond)
	}

	call(t, r.mcp, "call_tool", map[string]any{"name": "files.echo"})
	// The next report is due a day after the first; once its send has run,
	// the one after is armed.
	clock.waitArmed(t, first.Add(24*time.Hour))
	clock.advance(24 * time.Hour)
	clock.waitArmed(t, first.Add(48*time.Hour))

	if n := len(rs.list()); n != 1 {
		t.Errorf("collector received %d reports after telemetry was turned off, want 1", n)
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	for _, err := range rs.errs {
		t.Errorf("collector could not decode a report: %v", err)
	}
}

// TestTurningTelemetryOnWhileRunningSendsTheNextReport: a daemon started
// with telemetry off counts nothing; a reload that turns it on counts from
// then, and the next scheduled report carries it (ADR 0020).
func TestTurningTelemetryOnWhileRunningSendsTheNextReport(t *testing.T) {
	rs, srv := collect(t)
	h := newHost(t)
	getenv := h.env.Getenv
	h.env.Getenv = func(k string) string {
		if k == telemetry.EndpointEnv {
			return srv.URL
		}

		return getenv(k)
	}
	clock := newFakeClock()
	opts := h.options(t)
	opts.Version, opts.Commit = "v0.4.0", "2e3a922"
	opts.Clock = clock
	opts.Jitter = func() float64 { return 0 }
	opts.ReportDifficulty = 1
	listen := freeAddr(t)
	files := stdioConnector(t, fakeupstream.V20251125)
	cfg := func(enabled bool, retention string) map[string]any {
		return map[string]any{
			"server":     map[string]any{"listen": listen},
			"calls":      map[string]any{"retention": retention},
			"telemetry":  map[string]any{"enabled": enabled},
			"connectors": map[string]any{"files": files},
		}
	}
	r := serve(t, h, opts, cfg(false, "1h"))
	r.waitStatus(t, "files synced", synced("files"))

	// Off: not counted.
	call(t, r.mcp, "call_tool", map[string]any{"name": "files.echo"})

	// The retention change marks when the reload has applied: Reload applies
	// telemetry before it.
	h.writeConfig(t, cfg(true, "2h"))
	deadline := time.Now().Add(15 * time.Second)

	for r.calls(t).Retention != "2h0m0s" {
		if time.Now().After(deadline) {
			t.Fatal("the reload never applied")
		}

		time.Sleep(10 * time.Millisecond)
	}

	call(t, r.mcp, "call_tool", map[string]any{"name": "files.echo"})
	clock.waitArmed(t, epoch.Add(5*time.Minute))
	clock.advance(5 * time.Minute)
	got := rs.wait(t, 1)[0]

	if got.Calls.OK != 1 {
		t.Errorf("report counts %d ok calls, want 1: only the call after telemetry was turned on", got.Calls.OK)
	}

	if _, err := os.Stat(h.paths.TelemetryIDFile()); err != nil {
		t.Errorf("install ID file after the first report: %v", err)
	}
}
