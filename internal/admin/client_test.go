package admin_test

import (
	"context"
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// TestWaitConnector covers when the wait for a new connector ends: when it
// is listed and past starting, on a config error the daemon didn't have
// before, and on the time limit, which a stale config error doesn't cut
// short.
func TestWaitConnector(t *testing.T) {
	ready := upstream.ConnectorStatus{Connector: "fs", State: upstream.StateReady}
	starting := upstream.ConnectorStatus{Connector: "fs", State: upstream.StateStarting}

	for _, tt := range []struct {
		name          string
		states        []upstream.ConnectorStatus
		configErr     string
		prevErr       string
		wantState     upstream.State
		wantConfigErr string
		wantTimedOut  bool
	}{
		{name: "settled", states: []upstream.ConnectorStatus{ready}, wantState: upstream.StateReady},
		{name: "still starting", states: []upstream.ConnectorStatus{starting}, wantTimedOut: true},
		{name: "not loaded yet", wantTimedOut: true},
		{name: "new config error", configErr: "bad toml", wantConfigErr: "bad toml"},
		{name: "stale config error", configErr: "bad toml", prevErr: "bad toml", wantTimedOut: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, user := newStore(t)
			c, _ := serve(t, options(t, st, user, &fakeUpstreams{states: tt.states},
				admin.ConfigState{ConfigError: tt.configErr}))

			got, err := c.WaitConnector(t.Context(), "fs", tt.prevErr, 10*time.Millisecond, 100*time.Millisecond)

			if err != nil {
				t.Fatalf("WaitConnector: %v", err)
			}

			if got.Connector.State != tt.wantState || got.ConfigError != tt.wantConfigErr || got.TimedOut != tt.wantTimedOut {
				t.Errorf("WaitConnector = (state %q, config error %q, timed out %v), want (%q, %q, %v)",
					got.Connector.State, got.ConfigError, got.TimedOut, tt.wantState, tt.wantConfigErr, tt.wantTimedOut)
			}
		})
	}
}

// TestWaitConnectorCancelled: a cancelled ctx is an error, not a timeout.
func TestWaitConnectorCancelled(t *testing.T) {
	st, user := newStore(t)
	c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if got, err := c.WaitConnector(ctx, "fs", "", 10*time.Millisecond, time.Second); err == nil {
		t.Errorf("WaitConnector = %+v, nil; want an error", got)
	}
}
