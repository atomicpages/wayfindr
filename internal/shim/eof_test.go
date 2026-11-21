//go:build !windows

package shim_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// pipedResponse is one line the shim wrote to stdout.
type pipedResponse struct {
	ID     int `json:"id"`
	Result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// pipe runs `wayfindr stdio` as a script would: every line on stdin, which
// is closed right after them. It returns the responses by ID and fails the
// test unless the shim exits 0 within a minute.
func (m machine) pipe(t *testing.T, name string, lines ...string) map[int]pipedResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, m.bin, "stdio") //nolint:gosec // G204: the binary this test built
	cmd.Env = m.env
	cmd.Dir = m.project
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderr, err := os.Create(filepath.Join(m.logs, name+".stderr")) //nolint:gosec // inside the test's temp dir

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = stderr.Close() })
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("wayfindr stdio with stdin closed: %v\nstdout:\n%s\n%s", err, stdout.String(), m.logOf(name))
	}

	got := make(map[int]pipedResponse)
	sc := bufio.NewScanner(&stdout)

	for sc.Scan() {
		var r pipedResponse

		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("stdout line %q: %v", sc.Text(), err)
		}

		got[r.ID] = r
	}

	return got
}

// initializeLine is an initialize request, ID 1, at 2025-11-25 with the
// given client capabilities.
func initializeLine(capabilities string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":` +
		capabilities + `,"clientInfo":{"name":"script","version":"v0"}}}`
}

const initializedLine = `{"jsonrpc":"2.0","method":"notifications/initialized"}`

// A script that pipes its requests in and closes stdin at once still gets
// every answer before the shim exits (wayfindr-4qmt): the SDK alone would
// end the session at EOF and drop the requests still in flight.
func TestShimAnswersEveryRequestWhenStdinClosesRightAfterThem(t *testing.T) {
	m := newMachine(t)
	got := m.pipe(t, "script",
		initializeLine(`{}`),
		initializedLine,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_tools","arguments":{"query":"issue"}}}`,
	)

	var ids []int

	for id, r := range got {
		if r.Error != nil {
			t.Errorf("response %d is an error: %s", id, r.Error.Message)
		}

		ids = append(ids, id)
	}

	slices.Sort(ids)

	if diff := cmp.Diff([]int{1, 2, 3}, ids); diff != "" {
		t.Errorf("responses on stdout (-want +got):\n%s", diff)
	}
}

// A call that needs the user's input while stdin is already closed can't
// be asked: it gets the usual isError result rather than hanging the shim.
func TestShimAnswersACallNeedingInputAfterStdinCloses(t *testing.T) {
	m := newMachineAt(t, fakeupstream.V20251125, fakeupstream.WithElicitation())
	waitForIssueTool(t, m.startShim(t, "warm"))

	got := m.pipe(t, "script",
		initializeLine(`{"elicitation":{}}`),
		initializedLine,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"call_tool","arguments":{"name":"issues.`+fakeupstream.ToolElicit+`"}}}`,
	)
	r, ok := got[2]

	if !ok {
		t.Fatalf("no response to the call; got %+v", got)
	}

	if r.Error != nil || !r.Result.IsError || len(r.Result.Content) != 1 || !strings.Contains(r.Result.Content[0].Text, "could not be asked") {
		t.Errorf("response = %+v, want an isError result saying the client could not be asked", r)
	}
}
