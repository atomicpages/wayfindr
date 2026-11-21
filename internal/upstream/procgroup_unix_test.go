//go:build unix

package upstream_test

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// goneWithin reports whether process pid stops existing within d. An
// orphaned grandchild is reaped by init, not by the manager, so it may
// linger briefly as a zombie after it is killed.
func goneWithin(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)

	for alive(pid) {
		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(10 * time.Millisecond)
	}

	return true
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
			t.Cleanup(func() {
				// Don't leak processes when the test fails.
				_ = syscall.Kill(r.PID, syscall.SIGKILL)
				_ = syscall.Kill(r.Grandchild, syscall.SIGKILL)
			})

			if !alive(r.PID) || r.Grandchild == 0 || !alive(r.Grandchild) {
				t.Fatalf("child %d alive %v, grandchild %d alive %v; want both running after Apply",
					r.PID, alive(r.PID), r.Grandchild, alive(r.Grandchild))
			}

			tt.stop(t, m, clock)

			if !goneWithin(r.PID, 2*time.Second) {
				t.Errorf("child %d still running after %s", r.PID, tt.name)
			}

			if !goneWithin(r.Grandchild, 2*time.Second) {
				t.Errorf("grandchild %d still running after %s", r.Grandchild, tt.name)
			}
		})
	}
}
