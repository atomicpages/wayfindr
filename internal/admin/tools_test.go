package admin_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

func TestToolsReturnsTheListing(t *testing.T) {
	st, user := newStore(t)
	opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
	want := admin.ToolList{Connector: "gmail", Tools: []admin.ToolEntry{
		{Name: "create_draft", Class: "write", Exposed: true},
		{Name: "send_message", Class: "write"},
	}}

	var got string

	opts.Tools = func(_ context.Context, connector string) (admin.ToolList, error) {
		got = connector

		return want, nil
	}
	c, _ := serve(t, opts)

	list, err := c.Tools(t.Context(), "gmail")

	if err != nil {
		t.Fatalf("Tools: %v", err)
	}

	if got != "gmail" {
		t.Errorf("lister got connector %q, want gmail", got)
	}

	if diff := cmp.Diff(want, list); diff != "" {
		t.Errorf("listing (-want +got):\n%s", diff)
	}
}

// A listing the daemon can't make answers with the advice, and a status
// that says whether the connector is unknown or not ready.
func TestToolsRefusals(t *testing.T) {
	advice := admin.Advice{Summary: "The upstream cannot be reached.", Action: "Check that it is running."}
	tests := []struct {
		name   string
		tools  func(context.Context, string) (admin.ToolList, error)
		status int
		want   string
	}{
		{"no lister", nil, http.StatusNotImplemented, "tool listing is not available"},
		{"unknown connector", func(context.Context, string) (admin.ToolList, error) {
			return admin.ToolList{}, &admin.ToolsError{Kind: admin.KindUnknownConnector, Advice: admin.Advice{Summary: "No connector is called gmail."}}
		}, http.StatusNotFound, "No connector is called gmail."},
		{"not ready", func(context.Context, string) (admin.ToolList, error) {
			return admin.ToolList{}, &admin.ToolsError{Kind: "unreachable", Advice: advice}
		}, http.StatusServiceUnavailable, "The upstream cannot be reached. Check that it is running."},
		{"failed", func(context.Context, string) (admin.ToolList, error) {
			return admin.ToolList{}, errors.New("boom")
		}, http.StatusInternalServerError, "listing tools failed; see the daemon log"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, user := newStore(t)
			opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
			opts.Tools = tt.tools
			c, _ := serve(t, opts)

			if _, err := c.Tools(t.Context(), "gmail"); err == nil || statusOf(err) != tt.status || err.Error() != tt.want {
				t.Errorf("Tools error = %v (status %d), want %d %q", err, statusOf(err), tt.status, tt.want)
			}
		})
	}
}
