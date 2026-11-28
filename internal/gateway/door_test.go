package gateway_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// TestCallRefusesWhatCallToolRefuses: the admin API's door narrows as /mcp
// does. A tool the live tools list hides is refused even though the catalog
// still holds it, and a connector a scoped token leaves out is refused as
// unconfigured; the call log names the client and the token the door
// passed in.
func TestCallRefusesWhatCallToolRefuses(t *testing.T) {
	rec := &recorder{}
	policy := func(string) config.ToolPolicy { return config.ToolPolicy{"@read"} }
	e := newEnvWith(t, envOptions{
		fakes: map[string][]fakeupstream.Option{
			"gmail":  {gmailTools()},
			"linear": {fakeupstream.WithTools(createIssue())},
		},
		// Synced with every tool exposed, so the catalog holds send_message
		// and only the live policy hides it.
		gateway: gateway.Options{Calls: rec, Policy: policy},
	})
	nightly := clienttoken.Client{Name: "nightly", Connectors: []string{"linear"}}

	optsFor := func(c clienttoken.Client) gateway.CallOptions {
		return gateway.CallOptions{
			Client:     c,
			ClientName: "wayfindr-cli",
			Narrow:     func() (config.Narrowing, error) { return config.Narrowing{Token: c.Connectors}, nil },
			Asker:      gateway.Asker{ProtocolVersion: gateway.NewestProtocolVersion},
		}
	}

	tests := []struct {
		name   string
		client clienttoken.Client
		tool   string
		want   store.Call
		text   string
	}{
		{"hidden tool", clienttoken.Default(), "gmail.send_message",
			store.Call{Client: "wayfindr-cli", TokenName: "default", Connector: "gmail", Tool: "send_message",
				Result: store.CallRejected, ErrorKind: "unknown_tool", ErrorCase: "hidden_by_policy"},
			hiddenText},
		{"connector the token leaves out", nightly, "gmail.get_thread",
			store.Call{Client: "wayfindr-cli", TokenName: "nightly", Connector: "gmail", Tool: "get_thread",
				Result: store.CallRejected, ErrorKind: "unknown_tool", ErrorCase: "no_connector"},
			"no connector is called gmail"},
		{"exposed tool", clienttoken.Default(), "gmail.get_thread",
			store.Call{Client: "wayfindr-cli", TokenName: "default", Connector: "gmail", Tool: "get_thread", Result: store.CallOK},
			""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec.take()
			res := e.gateway.Call(t.Context(), gateway.CallRequest{Name: tt.tool}, optsFor(tt.client)).Result

			if got := text(t, res); res.IsError != (tt.text != "") || !strings.Contains(got, tt.text) {
				t.Errorf("result (isError %v) %q, want an error containing %q (or success for \"\")", res.IsError, got, tt.text)
			}

			got := rec.take()

			if len(got) != 1 {
				t.Fatalf("recorded %d calls, want 1", len(got))
			}

			// ResultBytes is TestCallToolRecordsTheSizeOfTheResult's; this test is about the door.
			opts := cmpopts.IgnoreFields(store.Call{}, "UserID", "StartedAt", "Duration", "ErrorText", "ResultBytes")

			if diff := cmp.Diff(tt.want, got[0], opts); diff != "" {
				t.Errorf("call log row (-want +got):\n%s", diff)
			}
		})
	}
}

// TestClassReadsTheCatalog: Class is the catalog entry's class, by
// call_tool's rule, and false for a tool the catalog lacks.
func TestClassReadsTheCatalog(t *testing.T) {
	e := newEnv(t, map[string][]fakeupstream.Option{"gmail": {gmailTools()}})

	tests := []struct {
		tool   string
		want   config.ToolClass
		wantOK bool
	}{
		{"get_thread", config.ClassRead, true},
		{"send_message", config.ClassWrite, true},
		{"purge", config.ClassDestructive, true},
		{"nope", "", false},
	}

	for _, tt := range tests {
		got, ok := e.gateway.Class(t.Context(), "gmail", tt.tool)

		if got != tt.want || ok != tt.wantOK {
			t.Errorf("Class(gmail, %s) = %q, %v, want %q, %v", tt.tool, got, ok, tt.want, tt.wantOK)
		}
	}
}
