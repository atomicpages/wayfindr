package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/uuid"

	"github.com/atomicpages/wayfindr/internal/store"
)

func ms(n int64) *time.Duration {
	d := time.Duration(n) * time.Millisecond

	return &d
}

// callsAt returns the fixture calls of user: one per result, a second
// apart from base, oldest first. Client and TokenName vary independently, each
// empty in some calls. ResultBytes is set in two, one of them 0, and nil in
// the rest, so the round trip tells 0 from NULL.
func callsAt(user string, base time.Time) []store.Call {
	return []store.Call{
		{UserID: user, Client: "claude-code", TokenName: "default", Connector: "linear", Tool: "create_issue",
			StartedAt: base, Duration: ms(412), Result: store.CallOK, ResultBytes: ptr(int64(1_258_291))},
		{UserID: user, TokenName: "default", Connector: "github", Tool: "get_file",
			StartedAt: base.Add(time.Second), Result: store.CallRejected,
			ErrorKind: "invalid_arguments", ErrorCase: "schema_mismatch",
			ErrorText: "The arguments do not match github.get_file's inputSchema. To fix: Fix the arguments and call again."},
		{UserID: user, Client: "cursor", TokenName: "nightly-triage", Connector: "slack", Tool: "post_message",
			StartedAt: base.Add(2 * time.Second), Duration: ms(30_000), Result: store.CallFailed, ErrorKind: "timeout"},
		{UserID: user, Client: "cursor", Connector: "linear", Tool: "search_issues",
			StartedAt: base.Add(3 * time.Second), Duration: ms(230), Result: store.CallToolError,
			ErrorText: "Unknown filter: state=opne", ResultBytes: ptr(int64(0))},
	}
}

func ptr[T any](v T) *T { return &v }

func recordCalls(t *testing.T, s store.Store, calls []store.Call) {
	t.Helper()

	for _, c := range calls {
		if err := s.RecordCall(context.Background(), c); err != nil {
			t.Fatalf("RecordCall(%s.%s): %v", c.Connector, c.Tool, err)
		}
	}
}

func listCalls(t *testing.T, s store.Store, f store.CallFilter) []store.Call {
	t.Helper()
	got, err := s.ListCalls(context.Background(), f)

	if err != nil {
		t.Fatalf("ListCalls(%+v): %v", f, err)
	}

	return got
}

// ignoreID compares calls without their store-generated IDs.
func ignoreID() cmp.Option { return cmpopts.IgnoreFields(store.Call{}, "ID") }

func reversed(calls []store.Call) []store.Call {
	out := make([]store.Call, 0, len(calls))

	for i := len(calls) - 1; i >= 0; i-- {
		out = append(out, calls[i])
	}

	return out
}

func testRecordCallRoundTripsNewestFirstWithUUIDv7IDs(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	calls := callsAt(user, time.UnixMilli(1_762_000_000_000))
	recordCalls(t, s, calls)

	got := listCalls(t, s, store.CallFilter{UserID: user, Limit: 10})

	if diff := cmp.Diff(reversed(calls), got, ignoreID()); diff != "" {
		t.Fatalf("ListCalls (-want +got):\n%s", diff)
	}

	seen := map[string]bool{}

	for _, c := range got {
		id, err := uuid.Parse(c.ID)

		if err != nil || id.Version() != 7 {
			t.Errorf("ID %q is not a UUIDv7 (err %v)", c.ID, err)
		}

		if seen[c.ID] {
			t.Errorf("ID %q repeats", c.ID)
		}

		seen[c.ID] = true
	}
}

func testRecordCallKeepsMillisecondsOfStartedAt(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	at := time.UnixMilli(1_762_000_000_123).Add(456 * time.Microsecond)
	recordCalls(t, s, []store.Call{{UserID: user, Connector: "linear", Tool: "x", StartedAt: at, Result: store.CallOK}})

	got := listCalls(t, s, store.CallFilter{UserID: user, Limit: 1})

	if len(got) != 1 || !got[0].StartedAt.Equal(time.UnixMilli(1_762_000_000_123)) {
		t.Fatalf("StartedAt = %v, want %v", got, time.UnixMilli(1_762_000_000_123))
	}
}

func testListCallsFilters(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)
	base := time.UnixMilli(1_762_000_000_000)
	calls := callsAt(user, base)
	recordCalls(t, s, calls)
	// Another user's calls are never listed.
	recordCalls(t, s, callsAt(unknownUser(t), base))

	tests := []struct {
		name   string
		filter store.CallFilter
		want   []store.Call
	}{
		{"since is inclusive", store.CallFilter{Since: base.Add(2 * time.Second), Limit: 10},
			[]store.Call{calls[3], calls[2]}},
		{"errors only", store.CallFilter{ErrorsOnly: true, Limit: 10},
			[]store.Call{calls[3], calls[2], calls[1]}},
		{"one connector", store.CallFilter{Connectors: []string{"linear"}, Limit: 10},
			[]store.Call{calls[3], calls[0]}},
		{"two connectors", store.CallFilter{Connectors: []string{"github", "slack"}, Limit: 10},
			[]store.Call{calls[2], calls[1]}},
		{"limit keeps the newest", store.CallFilter{Limit: 2},
			[]store.Call{calls[3], calls[2]}},
		{"filters combine", store.CallFilter{Connectors: []string{"linear"}, ErrorsOnly: true, Limit: 10},
			[]store.Call{calls[3]}},
		{"nothing matches", store.CallFilter{Connectors: []string{"gmail"}, Limit: 10}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.filter
			f.UserID = user

			if diff := cmp.Diff(tt.want, listCalls(t, s, f), ignoreID(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ListCalls (-want +got):\n%s", diff)
			}
		})
	}
}

func testListCallsRejectsNonPositiveLimit(t *testing.T, newDatabase NewDatabase) {
	s, user := newStore(t, newDatabase)

	for _, limit := range []int{0, -1} {
		if _, err := s.ListCalls(context.Background(), store.CallFilter{UserID: user, Limit: limit}); err == nil {
			t.Errorf("ListCalls(limit %d): err = nil, want an error", limit)
		}
	}
}

func testPruneCallsDeletesOnlyTheUsersOlderCalls(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	s, user := newStore(t, newDatabase)
	other := unknownUser(t)
	base := time.UnixMilli(1_762_000_000_000)
	calls := callsAt(user, base)
	recordCalls(t, s, calls)
	recordCalls(t, s, callsAt(other, base))

	// before is exclusive: the call at base+2s stays.
	n, err := s.PruneCalls(ctx, user, base.Add(2*time.Second))

	if err != nil {
		t.Fatalf("PruneCalls: %v", err)
	}

	if n != 2 {
		t.Errorf("PruneCalls deleted %d, want 2", n)
	}

	if diff := cmp.Diff([]store.Call{calls[3], calls[2]}, listCalls(t, s, store.CallFilter{UserID: user, Limit: 10}), ignoreID()); diff != "" {
		t.Errorf("user's calls after prune (-want +got):\n%s", diff)
	}

	if got := listCalls(t, s, store.CallFilter{UserID: other, Limit: 10}); len(got) != 4 {
		t.Errorf("other user's calls after prune = %d, want 4", len(got))
	}
}
