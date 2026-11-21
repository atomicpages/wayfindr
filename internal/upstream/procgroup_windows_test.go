//go:build windows

package upstream_test

import (
	"context"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// This test mirrors procgroup_unix_test.go and runs in CI's Windows job;
// development is on unix. The grandchild starts while the child starts
// up, which is the gap starting the child suspended closes, but nothing
// forces it into that gap, so passing shows grandchildren join the job, not
// that the gap is closed.

// openProcess opens process pid for waiting on, and kills it when the test
// ends so a failing test doesn't leak it. Holding the handle keeps pid from
// being reused while the test runs.
func openProcess(t *testing.T, pid int) windows.Handle {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid)) //nolint:gosec // G115: a PID fits in a DWORD.

	if err != nil {
		t.Fatalf("open process %d: %v", pid, err)
	}

	t.Cleanup(func() {
		_ = windows.TerminateProcess(h, 1)
		_ = windows.CloseHandle(h)
	})

	return h
}

// exitsWithin reports whether the process h refers to exits within d. A d
// of 0 just checks whether it has exited.
func exitsWithin(t *testing.T, h windows.Handle, d time.Duration) bool {
	t.Helper()
	ev, err := windows.WaitForSingleObject(h, uint32(d.Milliseconds())) //nolint:gosec // G115: test durations are short.

	if err != nil {
		t.Fatalf("wait for process: %v", err)
	}

	return ev == windows.WAIT_OBJECT_0
}

func TestStoppingStdioChildStopsItsGrandchildren(t *testing.T) {
	tests := []struct {
		name string
		stop func(t *testing.T, m *upstream.Manager, clock *fakeClock)
	}{
		{
			name: "Close",
			stop: func(t *testing.T, m *upstream.Manager, _ *fakeClock) {
				if err := m.Close(context.Background()); err != nil {
					t.Fatalf("Close: %v", err)
				}
			},
		},
		{
			name: "reap",
			stop: func(_ *testing.T, m *upstream.Manager, clock *fakeClock) {
				clock.Advance(10*time.Minute + time.Second)
				m.Reap()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newFakeClock()
			conn, report := reportingConnector(t)
			conn.Env[grandchildEnv] = "1"
			m, _ := newManager(t, nil, withClock(clock))
			apply(context.Background(), m, nil, config.Connectors{"gh": conn})
			r := readReport(t, report)

			if r.Grandchild == 0 {
				t.Fatalf("child %d reported no grandchild", r.PID)
			}

			child := openProcess(t, r.PID)
			grandchild := openProcess(t, r.Grandchild)

			if exitsWithin(t, child, 0) || exitsWithin(t, grandchild, 0) {
				t.Fatalf("child %d or grandchild %d already exited; want both running after Apply", r.PID, r.Grandchild)
			}

			tt.stop(t, m, clock)

			if !exitsWithin(t, child, 2*time.Second) {
				t.Errorf("child %d still running after %s", r.PID, tt.name)
			}

			if !exitsWithin(t, grandchild, 2*time.Second) {
				t.Errorf("grandchild %d still running after %s", r.Grandchild, tt.name)
			}
		})
	}
}
