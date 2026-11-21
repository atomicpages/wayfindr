package gateway_test

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// livePolicy is a tool policy a test can change while the gateway runs, as
// a reload does.
type livePolicy struct {
	mu sync.Mutex
	p  config.ToolPolicy
}

func (l *livePolicy) set(p config.ToolPolicy) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.p = p
}

func (l *livePolicy) get(string) config.ToolPolicy {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.p
}

// gmailTools are one tool of each class.
func gmailTools() fakeupstream.Option {
	no := false

	return fakeupstream.WithTools(
		&mcp.Tool{Name: "get_thread", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		&mcp.Tool{Name: "send_message", Annotations: &mcp.ToolAnnotations{DestructiveHint: &no}},
		&mcp.Tool{Name: "purge"},
	)
}

const (
	hiddenText   = "is hidden by the tools list of [connectors.gmail]"
	notFoundText = "in the catalog"
)

// wantCallText calls tool through call_tool and checks the result: an
// isError result containing want, or success for want "".
func wantCallText(t *testing.T, cs *mcp.ClientSession, tool, want string) {
	t.Helper()
	res := call(t, cs, "call_tool", map[string]any{"name": tool})
	got := text(t, res)

	switch {
	case want == "" && res.IsError:
		t.Errorf("%s: isError result %q, want success", tool, got)
	case want != "" && (!res.IsError || !strings.Contains(got, want)):
		t.Errorf("%s: result (isError %v) %q, want an error containing %q", tool, res.IsError, got, want)
	}
}

// TestCallToolRefusesToolsTheSyncHid: tools the policy hid at sync are not
// in the catalog, and call_tool says the config hid them, whether the
// policy hid the name or the class, rather than that they don't exist.
func TestCallToolRefusesToolsTheSyncHid(t *testing.T) {
	policy := func(string) config.ToolPolicy { return config.ToolPolicy{"@read"} }
	e := newEnvWith(t, envOptions{
		fakes:      map[string][]fakeupstream.Option{"gmail": {gmailTools()}},
		syncPolicy: policy,
		gateway:    gateway.Options{Policy: policy},
	})
	cs := connect(t, e.url, fakeupstream.V20251125)

	wantCallText(t, cs, "gmail.get_thread", "")
	wantCallText(t, cs, "gmail.send_message", hiddenText)
	wantCallText(t, cs, "gmail.purge", hiddenText)
	wantCallText(t, cs, "gmail.nope", notFoundText)
}

// TestCallToolRefusesToolsAReloadHid: until the re-sync, the catalog still
// holds tools a reload just hid; call_tool checks the live policy, by name
// and by class.
func TestCallToolRefusesToolsAReloadHid(t *testing.T) {
	live := &livePolicy{p: config.ToolPolicy{"@write", "!purge"}}
	e := newEnvWith(t, envOptions{
		fakes:   map[string][]fakeupstream.Option{"gmail": {gmailTools()}},
		gateway: gateway.Options{Policy: live.get},
	})
	cs := connect(t, e.url, fakeupstream.V20251125)

	wantCallText(t, cs, "gmail.send_message", "")
	wantCallText(t, cs, "gmail.purge", hiddenText)

	if got := searchNames(t, cs, "send message"); !slices.Contains(got, "gmail.send_message") {
		t.Fatalf("search_tools under the first policy = %v, want it to find gmail.send_message", got)
	}

	live.set(config.ToolPolicy{"@read"})
	wantCallText(t, cs, "gmail.get_thread", "")
	wantCallText(t, cs, "gmail.send_message", hiddenText)

	// search_tools doesn't return them either, by query or by name.
	var page struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Missing []string `json:"missing"`
	}

	if got := searchNames(t, cs, "send message"); slices.Contains(got, "gmail.send_message") {
		t.Errorf("search_tools = %v, want no gmail.send_message, which the policy hides", got)
	}

	res := call(t, cs, "search_tools", map[string]any{"names": []string{"gmail.get_thread", "gmail.send_message"}})

	if err := json.Unmarshal([]byte(text(t, res)), &page); err != nil {
		t.Fatalf("search_tools names: %v", err)
	}

	if diff := cmp.Diff([]string{"gmail.send_message"}, page.Missing); diff != "" {
		t.Errorf("names lookup missing (-want +got):\n%s", diff)
	}
}

// searchNames is the qualified names search_tools returns for query.
func searchNames(t *testing.T, cs *mcp.ClientSession, query string) []string {
	t.Helper()

	var page struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}

	if err := json.Unmarshal([]byte(text(t, call(t, cs, "search_tools", map[string]any{"query": query}))), &page); err != nil {
		t.Fatalf("search_tools: %v", err)
	}

	var names []string

	for _, tl := range page.Tools {
		names = append(names, tl.Name)
	}

	return names
}

// TestCallToolDoesNotBlameThePolicyForAToolItNowExposes: after a reload
// exposes a tool the last sync hid, call_tool must not say the config
// hides it while the re-sync is pending.
func TestCallToolDoesNotBlameThePolicyForAToolItNowExposes(t *testing.T) {
	e := newEnvWith(t, envOptions{
		fakes:      map[string][]fakeupstream.Option{"gmail": {gmailTools()}},
		syncPolicy: func(string) config.ToolPolicy { return config.ToolPolicy{"@read"} },
		gateway:    gateway.Options{Policy: func(string) config.ToolPolicy { return config.ToolPolicy{"@read", "send_message"} }},
	})
	cs := connect(t, e.url, fakeupstream.V20251125)

	wantCallText(t, cs, "gmail.send_message", notFoundText)
	wantCallText(t, cs, "gmail.purge", hiddenText)
}

// TestCallToolRefusesTheAnswerToAToolHiddenSince: the policy is checked
// again when the client answers what the call asked, so a tool hidden in
// between does not run.
func TestCallToolRefusesTheAnswerToAToolHiddenSince(t *testing.T) {
	live := &livePolicy{}
	e := newEnvWith(t, envOptions{
		fakes:   map[string][]fakeupstream.Option{"linear": {fakeupstream.WithElicitation()}},
		gateway: gateway.Options{Policy: live.get},
	})
	cs := connectWith(t, e.url, fakeupstream.V20260728, answering(true))

	res, err := cs.CallTool(t.Context(), elicitCall())

	if err != nil || !res.NeedsInput() {
		t.Fatalf("CallTool = %+v, %v; want input_required", res, err)
	}

	live.set(config.ToolPolicy{"!" + fakeupstream.ToolElicit})
	p := elicitCall()
	p.RequestState = res.RequestState
	p.InputResponses = mcp.InputResponseMap{}

	for id := range res.InputRequests {
		p.InputResponses[id] = &mcp.ElicitResult{Action: "accept", Content: map[string]any{"answer": "yes"}}
	}

	res, err = cs.CallTool(t.Context(), p)

	if err != nil {
		t.Fatalf("CallTool with the answer: %v", err)
	}

	if got := text(t, res); !res.IsError || !strings.Contains(got, "is hidden by the tools list of [connectors.linear]") {
		t.Errorf("result (isError %v) %q, want hidden_by_policy", res.IsError, got)
	}
}
