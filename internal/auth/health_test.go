package auth_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

func TestHealthWarnings(t *testing.T) {
	const link = "http://127.0.0.1:7777/auth/linear"
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	day, week := 24*time.Hour, 7*24*time.Hour
	reauth := "Open the link to authorize the connector again."
	ahead := "Authorize again before then to avoid an interruption."

	tests := []struct {
		name   string
		health auth.Health
		want   []auth.Warning
	}{
		{name: "no credential", health: auth.Health{}},
		{
			name:   "an expiring token that refreshes",
			health: auth.Health{AccessExpiry: now.Add(time.Minute), HasRefresh: true},
		},
		{
			name:   "a token that cannot be refreshed, more than a day out, is a note",
			health: auth.Health{AccessExpiry: now.Add(day + time.Second)},
			want: []auth.Warning{{
				Code: auth.WarningNoRefresh, At: now.Add(day + time.Second), Note: true,
				Advice: auth.Advice{
					Summary: "The access token expires at 2026-11-09T09:00:01Z and cannot be refreshed.",
					Action:  ahead, Link: link,
				},
			}},
		},
		{
			name:   "a token that cannot be refreshed, a day out, is a warning",
			health: auth.Health{AccessExpiry: now.Add(day)},
			want: []auth.Warning{{
				Code: auth.WarningNoRefresh, At: now.Add(day),
				Advice: auth.Advice{
					Summary: "The access token expires at 2026-11-09T09:00:00Z and cannot be refreshed.",
					Action:  ahead, Link: link,
				},
			}},
		},
		{
			name:   "a token that cannot be refreshed, already expired",
			health: auth.Health{AccessExpiry: now},
			want: []auth.Warning{{
				Code: auth.WarningNoRefresh, At: now,
				Advice: auth.Advice{
					Summary: "The access token expired at 2026-11-08T09:00:00Z and cannot be refreshed.",
					Action:  reauth, Link: link,
				},
			}},
		},
		{
			name:   "a refresh token ending more than a week out",
			health: auth.Health{HasRefresh: true, RefreshExpiry: now.Add(week + time.Second)},
		},
		{
			name:   "a refresh token ending a week out",
			health: auth.Health{AccessExpiry: now.Add(time.Hour), HasRefresh: true, RefreshExpiry: now.Add(week)},
			want: []auth.Warning{{
				Code: auth.WarningRefreshExpiring, At: now.Add(week),
				Advice: auth.Advice{Summary: "The refresh token expires at 2026-11-15T09:00:00Z.", Action: ahead, Link: link},
			}},
		},
		{
			name:   "a registered client ending more than a week out",
			health: auth.Health{RegistrationExpiry: now.Add(week + time.Second)},
		},
		{
			name:   "a registered client ending a week out",
			health: auth.Health{RegistrationExpiry: now.Add(week)},
			want: []auth.Warning{{
				Code: auth.WarningRegistrationExpiring, At: now.Add(week),
				Advice: auth.Advice{
					Summary: "The OAuth client Wayfindr registered for the connector expires at 2026-11-15T09:00:00Z.",
					Action:  "Before then, run `wayfindr auth logout linear`, then authorize again: Wayfindr registers a new client.",
				},
			}},
		},
		{
			name:   "a registered client already expired",
			health: auth.Health{RegistrationExpiry: now.Add(-time.Hour)},
			want: []auth.Warning{{
				Code: auth.WarningRegistrationExpiring, At: now.Add(-time.Hour),
				Advice: auth.Advice{
					Summary: "The OAuth client Wayfindr registered for the connector expired at 2026-11-08T08:00:00Z.",
					Action:  "Run `wayfindr auth logout linear`, then authorize again: Wayfindr registers a new client.",
				},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, tt.health.Warnings(now, "linear", link)); diff != "" {
				t.Errorf("Warnings (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWarningUrgent(t *testing.T) {
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		w    auth.Warning
		want bool
	}{
		{name: "a day out", w: auth.Warning{At: now.Add(24 * time.Hour)}, want: true},
		{name: "past", w: auth.Warning{At: now.Add(-time.Hour)}, want: true},
		{name: "more than a day out", w: auth.Warning{At: now.Add(24*time.Hour + time.Second)}},
		{name: "a note", w: auth.Warning{At: now.Add(time.Hour), Note: true}},
	}

	for _, tt := range tests {
		if got := tt.w.Urgent(now); got != tt.want {
			t.Errorf("%s: Urgent = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A refresh stores the refresh token's lifetime the server sent, read
// against the broker's clock.
func TestHealthReadsTheRefreshExpiryARefreshReturned(t *testing.T) {
	h := newBrokerHarness(t, fakeas.WithRefreshTokenLifetime(30*24*time.Hour))
	h.seed(t, "", 30*time.Second)

	if status, err := h.get(t); err != nil || status != 200 {
		t.Fatalf("get = %d, %v; want 200 after a refresh", status, err)
	}

	got, err := h.broker.Health(t.Context(), h.conn)

	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	if want := h.clock.Now().Add(30 * 24 * time.Hour); !got.RefreshExpiry.Equal(want) || !got.HasRefresh {
		t.Errorf("Health = %+v; want a refresh token ending %v", got, want)
	}
}

// The stored DCR client's expiry counts only while a refresh would use it:
// a client_id in the config replaces it.
func TestHealthReadsTheRegisteredClientsExpiry(t *testing.T) {
	expiry := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	h := newBrokerHarness(t, fakeas.WithClientSecretExpiry(expiry))
	h.seedDCR(t, time.Hour)

	got, err := h.broker.Health(t.Context(), h.conn)

	if err != nil || !got.RegistrationExpiry.Equal(expiry) {
		t.Errorf("Health = %+v, %v; want a registration ending %v", got, err, expiry)
	}

	h.conn.Auth.ClientID = testClientID

	if got, err := h.broker.Health(t.Context(), h.conn); err != nil || !got.RegistrationExpiry.IsZero() {
		t.Errorf("Health with a config client_id = %+v, %v; want no registration expiry", got, err)
	}
}
