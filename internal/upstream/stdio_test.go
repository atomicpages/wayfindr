package upstream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// Variables that make a stdio fake (a re-executed test binary, see TestMain)
// report on itself before it starts serving.
const (
	// reportEnv names a file the child writes its childReport to.
	reportEnv = "UPSTREAM_TEST_REPORT"
	// stderrEnv holds a line the child writes to its stderr.
	stderrEnv = "UPSTREAM_TEST_STDERR"
	// grandchildEnv, set to any value, makes the child start a grandchild
	// that outlives it, the way npx and uvx leave the real server running.
	grandchildEnv = "UPSTREAM_TEST_GRANDCHILD"
	// lingerEnv makes a re-executed test binary block until it is killed.
	// The grandchild runs with it.
	lingerEnv = "UPSTREAM_TEST_LINGER"
	// hangEnv names a file; while it exists, a newly spawned child blocks
	// without serving, so its connect never finishes.
	hangEnv = "UPSTREAM_TEST_HANG_IF_EXISTS"
)

// childReport is what a reporting stdio fake writes about itself.
type childReport struct {
	PID int      `json:"pid"`
	Env []string `json:"env"`
	// Grandchild is the PID of the grandchild started for grandchildEnv,
	// or 0.
	Grandchild int `json:"grandchild"`
}

// lingerIfGrandchild blocks until a signal ends the process, when it is a
// grandchild started for grandchildEnv.
func lingerIfGrandchild() {
	if os.Getenv(lingerEnv) == "" {
		return
	}

	for {
		time.Sleep(time.Hour)
	}
}

// hangIfAsked blocks until a signal ends the process, when the file hangEnv
// names exists.
func hangIfAsked() {
	path := os.Getenv(hangEnv)

	if path == "" {
		return
	}

	if _, err := os.Stat(path); err != nil { //nolint:gosec // the parent test names a file in its temp dir.
		return
	}

	for {
		time.Sleep(time.Hour)
	}
}

// reportIfChild does the reporting asked for by reportEnv and stderrEnv.
// It runs before the child serves, so a finished connect means the report
// is written.
func reportIfChild() {
	if line := os.Getenv(stderrEnv); line != "" {
		fmt.Fprintln(os.Stderr, line)
	}

	path := os.Getenv(reportEnv)

	if path == "" {
		return
	}

	r := childReport{PID: os.Getpid(), Env: os.Environ()}

	if os.Getenv(grandchildEnv) != "" {
		// The grandchild gets no stdio and is never waited for, so it is
		// left behind when the child exits, still in the child's process
		// group.
		//nolint:gosec // G204: re-executes this test binary.
		gc := exec.CommandContext(context.Background(), os.Args[0])
		gc.Env = []string{lingerEnv + "=1"}

		if err := gc.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "start grandchild: %v\n", err)
			os.Exit(1)
		}

		r.Grandchild = gc.Process.Pid
	}

	data, err := json.Marshal(r)

	if err == nil {
		err = os.WriteFile(path, data, 0o600) //nolint:gosec // the parent test names a file in its temp dir.
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "report: %v\n", err)
		os.Exit(1)
	}
}

// reportingConnector is a stdio fake that writes a childReport to the
// returned file each time it is spawned.
func reportingConnector(t *testing.T) (config.Connector, string) {
	t.Helper()
	c := stdioConnector(t, fakeupstream.V20251125)
	path := filepath.Join(t.TempDir(), "report.json")
	c.Env[reportEnv] = config.Template(path)

	return c, path
}

// readReport reads the report of the most recently spawned child.
func readReport(t *testing.T, path string) childReport {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // inside the test's temp dir.

	if err != nil {
		t.Fatalf("read child report: %v", err)
	}

	var r childReport

	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("decode child report: %v", err)
	}

	return r
}

// fakeClock is a clock that moves only when told to.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func withClock(c *fakeClock) func(*upstream.Options) {
	return func(o *upstream.Options) { o.Now = c.Now }
}

// use makes one use of connector's upstream.
func use(t *testing.T, m *upstream.Manager, connector string) {
	t.Helper()

	if _, err := m.ListTools(context.Background(), connector); err != nil {
		t.Fatalf("ListTools(%q): %v", connector, err)
	}
}

func TestStdioSpawnFiresToolsChanged(t *testing.T) {
	conn, report := reportingConnector(t)
	m, rec := newManager(t, nil)

	apply(context.Background(), m, nil, config.Connectors{"gh": conn})

	if got := statusOf(t, m, "gh").State; got != upstream.StateReady {
		t.Fatalf("state = %q, want ready", got)
	}

	if !alive(readReport(t, report).PID) {
		t.Error("child is not running after Apply")
	}

	if got := rec.toolsChangedOf("gh"); got != 1 {
		t.Errorf("OnToolsChanged calls after spawn = %d, want 1", got)
	}

	use(t, m, "gh")

	if got := rec.toolsChangedOf("gh"); got != 1 {
		t.Errorf("OnToolsChanged calls after a call on the running child = %d, want 1", got)
	}
}

func TestStdioIdleChildIsReapedAndRespawnedOnNextCall(t *testing.T) {
	clock := newFakeClock()
	conn, report := reportingConnector(t)
	m, rec := newManager(t, nil, withClock(clock))
	apply(context.Background(), m, nil, config.Connectors{"gh": conn})
	first := readReport(t, report).PID
	rec.reset()

	clock.Advance(10*time.Minute + time.Second)
	m.Reap()

	if alive(first) {
		t.Fatal("child still running after the reaper ran past the idle timeout")
	}

	if got := statusOf(t, m, "gh").State; got != upstream.StateReady {
		t.Errorf("state after reap = %q, want ready", got)
	}

	if got := rec.of("gh"); len(got) != 0 {
		t.Errorf("OnStateChanged after reap = %v, want none", got)
	}

	use(t, m, "gh")

	second := readReport(t, report).PID

	if second == first || !alive(second) {
		t.Errorf("call after reap: child pid %d (first was %d), alive %v; want a new running child", second, first, alive(second))
	}

	if got := rec.toolsChangedOf("gh"); got != 2 {
		t.Errorf("OnToolsChanged calls after respawn = %d, want 2 (one per spawn)", got)
	}

	if got := rec.of("gh"); len(got) != 0 {
		t.Errorf("OnStateChanged after respawn = %v, want none", got)
	}
}

func TestStdioChildEnvOmitsWayfindrVariables(t *testing.T) {
	// The daemon's own secrets must not reach upstream processes (design §9).
	t.Setenv("WAYFINDR_SECRET_KEY", "master-key")
	t.Setenv("WAYFINDR_CLIENT_TOKEN", "client-token")
	t.Setenv("wayfindr_lower", "x")
	t.Setenv("UPSTREAM_TEST_INHERITED", "kept")
	conn, report := reportingConnector(t)
	conn.Env["GH_TOKEN"] = "${secret:gh}"
	conn.Env["WAYFINDR_CONFIGURED"] = "configured"
	m, _ := newManager(t, env{"secret:gh": "gh-secret"})

	apply(context.Background(), m, nil, config.Connectors{"gh": conn})

	got := map[string]string{}

	for _, kv := range readReport(t, report).Env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}

	for _, k := range []string{"WAYFINDR_SECRET_KEY", "WAYFINDR_CLIENT_TOKEN", "wayfindr_lower"} {
		if v, ok := got[k]; ok {
			t.Errorf("child env has %s=%q, want it absent", k, v)
		}
	}

	want := map[string]string{
		"UPSTREAM_TEST_INHERITED": "kept",       // the rest of the daemon's env
		"GH_TOKEN":                "gh-secret",  // configured env, resolved
		"WAYFINDR_CONFIGURED":     "configured", // configured explicitly, so kept
	}

	for k, v := range want {
		if got[k] != v {
			t.Errorf("child env %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestStdioChildStderrIsLoggedWithConnector(t *testing.T) {
	var buf syncBuffer
	conn := stdioConnector(t, fakeupstream.V20251125)
	conn.Env[stderrEnv] = "child says hello"
	m, _ := newManager(t, nil, func(o *upstream.Options) {
		o.Logger = zerolog.New(&buf).Level(zerolog.DebugLevel)
	})
	apply(context.Background(), m, nil, config.Connectors{"gh": conn})

	// Close waits for the child, and for its stderr to be copied.
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for line := range strings.Lines(buf.String()) {
		var entry map[string]any

		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}

		if entry["stderr"] == "child says hello" {
			want := map[string]any{"level": "debug", "connector": "gh", "stderr": "child says hello", "message": "stdio stderr"}

			if diff := cmp.Diff(want, entry); diff != "" {
				t.Errorf("stderr log entry (-want +got):\n%s", diff)
			}

			return
		}
	}

	t.Errorf("no log entry carries the child's stderr; log:\n%s", buf.String())
}

func TestCloseLeavesNoStdioChildRunning(t *testing.T) {
	set := config.Connectors{}
	reports := map[string]string{}

	for _, name := range []string{"a", "b", "c"} {
		conn, report := reportingConnector(t)
		set[name] = conn
		reports[name] = report
	}

	m, _ := newManager(t, nil)
	apply(context.Background(), m, nil, set)
	pids := map[string]int{}

	for name, report := range reports {
		pids[name] = readReport(t, report).PID

		if !alive(pids[name]) {
			t.Fatalf("%s: child not running after Apply", name)
		}
	}

	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for name, pid := range pids {
		if alive(pid) {
			t.Errorf("%s: child %d still running after Close", name, pid)
		}
	}

	if _, err := m.ListTools(context.Background(), "a"); !isKind(err, upstream.KindUnknownConnector) {
		t.Errorf("ListTools after Close: err = %v, want unknown_connector", err)
	}
}

func TestRemovedStdioConnectorStopsItsChild(t *testing.T) {
	conn, report := reportingConnector(t)
	m, _ := newManager(t, nil)
	apply(context.Background(), m, nil, config.Connectors{"gh": conn})
	pid := readReport(t, report).PID

	apply(context.Background(), m, config.Connectors{"gh": conn}, config.Connectors{})

	if alive(pid) {
		t.Errorf("child %d still running after its connector was removed", pid)
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent writes and reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func TestReaperKeepsUpstreamsThatAreNotIdle(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20251125)
	tests := []struct {
		name string
		conn config.Connector
		// run uses connector "c" and moves the clock, starting just after
		// Apply connected it.
		run func(t *testing.T, m *upstream.Manager, clock *fakeClock)
	}{
		{
			name: "stdio idle for exactly the timeout",
			conn: stdioConnector(t, fakeupstream.V20251125),
			run: func(_ *testing.T, _ *upstream.Manager, clock *fakeClock) {
				clock.Advance(10 * time.Minute)
			},
		},
		{
			name: "stdio idle since its last call",
			conn: stdioConnector(t, fakeupstream.V20251125),
			run: func(t *testing.T, m *upstream.Manager, clock *fakeClock) {
				clock.Advance(9 * time.Minute)
				use(t, m, "c")
				clock.Advance(9 * time.Minute)
			},
		},
		{
			name: "stdio with a call in progress",
			conn: stdioConnector(t, fakeupstream.V20251125),
			run: func(t *testing.T, m *upstream.Manager, clock *fakeClock) {
				_, release, err := m.Acquire(context.Background(), "c")

				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}

				t.Cleanup(release)
				clock.Advance(time.Hour)
			},
		},
		{
			name: "http idle past the timeout",
			conn: config.Connector{URL: config.Template(srv.URL)},
			run: func(_ *testing.T, _ *upstream.Manager, clock *fakeClock) {
				clock.Advance(time.Hour)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newFakeClock()
			m, rec := newManager(t, nil, withClock(clock))
			apply(context.Background(), m, nil, config.Connectors{"c": tt.conn})
			before, release, err := m.Acquire(context.Background(), "c")

			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}

			release()
			spawns := rec.toolsChangedOf("c")

			tt.run(t, m, clock)
			m.Reap()

			after, release, err := m.Acquire(context.Background(), "c")

			if err != nil {
				t.Fatalf("Acquire after reap: %v", err)
			}

			release()

			if after != before {
				t.Error("session was replaced, want the reaper to keep it")
			}

			if got := rec.toolsChangedOf("c"); got != spawns {
				t.Errorf("OnToolsChanged calls = %d, want %d (no respawn)", got, spawns)
			}
		})
	}
}
