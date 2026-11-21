package upstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// A tools/list result's cache hint (ttlMs, 2026-07-28 cacheable lists)
// shows in the connector's status once ListTools has read it; the daemon's
// poll reads it from there (design §7.8 step 3).
func TestListToolsRecordsTheToolsCacheHint(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			srv := fakeupstream.StartStreamable(t, version, fakeupstream.WithToolsTTL(5*time.Minute))
			m, _ := newManager(t, nil)
			apply(context.Background(), m, nil, config.Connectors{"linear": {Name: "linear", URL: config.Template(srv.URL)}})

			if got := statusOf(t, m, "linear").ToolsTTL; got != 0 {
				t.Errorf("ToolsTTL before any ListTools = %v, want 0", got)
			}

			if _, err := m.ListTools(context.Background(), "linear"); err != nil {
				t.Fatalf("ListTools: %v", err)
			}

			if got := statusOf(t, m, "linear").ToolsTTL; got != 5*time.Minute {
				t.Errorf("ToolsTTL after ListTools = %v, want 5m", got)
			}
		})
	}
}

// Every page carries its own hint, and the list is only as fresh as its
// stalest page: the smallest hint wins. The fake gives every page the same
// one, so this pins that following cursors keeps it.
func TestListToolsKeepsTheCacheHintAcrossPages(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20260728, fiveTools(), fakeupstream.WithPageSize(2), fakeupstream.WithToolsTTL(90*time.Second))
	m, _ := newManager(t, nil)
	apply(context.Background(), m, nil, config.Connectors{"linear": {Name: "linear", URL: config.Template(srv.URL)}})

	tools, err := m.ListTools(context.Background(), "linear")

	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	if len(tools) != 5 {
		t.Errorf("ListTools returned %d tools, want 5", len(tools))
	}

	if got := statusOf(t, m, "linear").ToolsTTL; got != 90*time.Second {
		t.Errorf("ToolsTTL = %v, want 1m30s", got)
	}
}

// A server that sends no hint, or 0 ("immediately stale"), gives none.
func TestListToolsWithoutACacheHintRecordsNone(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20260728)
	m, _ := newManager(t, nil)
	apply(context.Background(), m, nil, config.Connectors{"linear": {Name: "linear", URL: config.Template(srv.URL)}})

	if _, err := m.ListTools(context.Background(), "linear"); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	if got := statusOf(t, m, "linear").ToolsTTL; got != 0 {
		t.Errorf("ToolsTTL = %v, want 0", got)
	}
}

// PeekTools, the admin API's lister, returns the same tools but leaves
// ToolsTTL alone: only a sync's list sets the poller's schedule.
func TestPeekToolsDoesNotRecordTheCacheHint(t *testing.T) {
	srv := fakeupstream.StartStreamable(t, fakeupstream.V20260728, fiveTools(), fakeupstream.WithPageSize(2), fakeupstream.WithToolsTTL(5*time.Minute))
	m, _ := newManager(t, nil)
	apply(context.Background(), m, nil, config.Connectors{"linear": {Name: "linear", URL: config.Template(srv.URL)}})

	tools, err := m.PeekTools(context.Background(), "linear")

	if err != nil {
		t.Fatalf("PeekTools: %v", err)
	}

	if len(tools) != 5 {
		t.Errorf("PeekTools returned %d tools, want 5", len(tools))
	}

	if got := statusOf(t, m, "linear").ToolsTTL; got != 0 {
		t.Errorf("ToolsTTL after PeekTools = %v, want 0", got)
	}

	if _, err := m.ListTools(context.Background(), "linear"); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	if got := statusOf(t, m, "linear").ToolsTTL; got != 5*time.Minute {
		t.Errorf("ToolsTTL after ListTools = %v, want 5m", got)
	}
}
