package daemon

import (
	"testing"
	"time"
)

// pollInterval is a pure decision table from design §7.8 step 3 (the
// jitter, and the bounds on a cache hint), so it is tested directly
// (CODING_STANDARDS: Tests); poll_test.go covers its effect through the
// running daemon.
func TestPollInterval(t *testing.T) {
	tests := []struct {
		name       string
		base, hint time.Duration
		j          float64
		want       time.Duration
	}{
		{"no hint, lowest draw: 10% early", time.Hour, 0, 0, 54 * time.Minute},
		{"no hint, middle draw: on time", time.Hour, 0, 0.5, time.Hour},
		{"no hint, near the top draw: nearly 10% late", time.Hour, 0, 0.99, time.Hour + 5*time.Minute + 52*time.Second + 800*time.Millisecond},
		{"hint, lowest draw: the hint", time.Hour, 5 * time.Minute, 0, 5 * time.Minute},
		{"hint, middle draw: 10% after the hint", time.Hour, 5 * time.Minute, 0.5, 5*time.Minute + 30*time.Second},
		{"hint longer than refresh_interval wins", time.Hour, 3 * time.Hour, 0, 3 * time.Hour},
		{"hint below the floor: one minute", time.Hour, time.Second, 0, time.Minute},
		{"hint above the ceiling: one day", time.Hour, 7 * 24 * time.Hour, 0, 24 * time.Hour},
		{"negative hint: none", time.Hour, -time.Second, 0.5, time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pollInterval(tt.base, tt.hint, tt.j); got != tt.want {
				t.Errorf("pollInterval(%v, %v, %v) = %v, want %v", tt.base, tt.hint, tt.j, got, tt.want)
			}
		})
	}
}
