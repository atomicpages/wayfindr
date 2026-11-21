package upstream_test

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// fiveTools gives a fake the tools t0 to t4, which a page size of two splits
// over three pages.
func fiveTools() fakeupstream.Option {
	return fakeupstream.WithTools(
		&mcp.Tool{Name: "t0"}, &mcp.Tool{Name: "t1"}, &mcp.Tool{Name: "t2"},
		&mcp.Tool{Name: "t3"}, &mcp.Tool{Name: "t4"},
	)
}

// listToolNames lists connector's tools and returns their names in order.
func listToolNames(t *testing.T, conn config.Connector) []string {
	t.Helper()
	m, _ := newManager(t, nil)
	apply(context.Background(), m, nil, config.Connectors{"c": conn})

	tools, err := m.ListTools(context.Background(), "c")

	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	var names []string

	for _, tool := range tools {
		names = append(names, tool.Name)
	}

	return names
}

func TestListToolsOverStdio(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			t.Parallel()

			if diff := cmp.Diff([]string{"echo"}, listToolNames(t, stdioConnector(t, version))); diff != "" {
				t.Errorf("tools (-want +got):\n%s", diff)
			}
		})
	}
}

func TestListToolsOverStdioFollowsCursors(t *testing.T) {
	for _, version := range fakeupstream.Versions() {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			conn := stdioConnector(t, version, fiveTools(), fakeupstream.WithPageSize(2))

			if diff := cmp.Diff([]string{"t0", "t1", "t2", "t3", "t4"}, listToolNames(t, conn)); diff != "" {
				t.Errorf("tools over three pages of two (-want +got):\n%s", diff)
			}
		})
	}
}

func TestListToolsOverSSE(t *testing.T) {
	for _, version := range sseVersions() {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			srv := fakeupstream.StartSSE(t, version)
			conn := config.Connector{URL: config.Template(srv.URL), Transport: config.TransportSSE}

			if diff := cmp.Diff([]string{"echo"}, listToolNames(t, conn)); diff != "" {
				t.Errorf("tools (-want +got):\n%s", diff)
			}
		})
	}
}

func TestListToolsOverSSEFollowsCursors(t *testing.T) {
	for _, version := range sseVersions() {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			srv := fakeupstream.StartSSE(t, version, fiveTools(), fakeupstream.WithPageSize(2))
			conn := config.Connector{URL: config.Template(srv.URL), Transport: config.TransportSSE}

			if diff := cmp.Diff([]string{"t0", "t1", "t2", "t3", "t4"}, listToolNames(t, conn)); diff != "" {
				t.Errorf("tools over three pages of two (-want +got):\n%s", diff)
			}
		})
	}
}
