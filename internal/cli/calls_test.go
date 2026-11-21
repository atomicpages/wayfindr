package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/store"
)

func millis(n int64) *int64 { return &n }

// sampleCalls is every way a row renders: ok in milliseconds and seconds,
// the default and a scoped token or none, no client, a refusal with a Case and error text, a tool error's
// text, a failure with only a Kind, and names
// the agent wrote without a connector or at all.
func sampleCalls() admin.Calls {
	at := time.Date(2026, 11, 8, 9, 14, 2, 0, time.UTC)

	return admin.Calls{Retention: "720h0m0s", Calls: []admin.Call{
		{StartedAt: at, Client: "claude-code", TokenName: "default", Connector: "linear", Tool: "create_issue", DurationMS: millis(412), Result: store.CallOK},
		{StartedAt: at.Add(-22 * time.Second), Client: "claude-code", TokenName: "default", Connector: "linear", Tool: "search_issues",
			DurationMS: millis(1234), Result: store.CallToolError, Error: "Unknown filter: state=opne"},
		{StartedAt: at.Add(-111 * time.Second), Connector: "github", Tool: "get_file", Result: store.CallRejected,
			Kind: "invalid_arguments", Case: "schema_mismatch", Error: "The arguments do not match github.get_file's inputSchema. To fix: Fix the arguments and call again."},
		{StartedAt: at.Add(-19 * time.Minute), Client: "cursor", TokenName: "nightly-triage", Connector: "slack", Tool: "post_message",
			DurationMS: millis(30_000), Result: store.CallFailed, Kind: "timeout"},
		{StartedAt: at.Add(-20 * time.Minute), Tool: "create_issue", Result: store.CallRejected, Kind: "invalid_name", Case: "unqualified"},
		{StartedAt: at.Add(-21 * time.Minute), Result: store.CallRejected, Kind: "invalid_name", Case: "missing_name"},
	}}
}

// The goldens pin how the call log renders. The renderers get sampleCalls
// directly; the tests below run the command against the admin API.
func TestCallsOutputMatchesGolden(t *testing.T) {
	for _, tt := range []struct {
		name   string
		write  func(io.Writer, admin.Calls) error
		golden string
	}{
		{"table", writeCallsTable, "calls.golden"},
		{"json", writeCallsJSON, "calls.json.golden"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer

			if err := tt.write(&stdout, sampleCalls()); err != nil {
				t.Fatal(err)
			}

			want, err := os.ReadFile(filepath.Join("testdata", tt.golden))

			if err != nil {
				t.Fatal(err)
			}

			if diff := cmp.Diff(string(want), stdout.String()); diff != "" {
				t.Errorf("output differs from testdata/%s (-want +got):\n%s", tt.golden, diff)
			}
		})
	}
}

// callsDaemon serves the admin API over st, with the call log's retention,
// and writes the test host's config and token for it.
func callsDaemon(t *testing.T, st *fakeStore, retention time.Duration) Host {
	t.Helper()
	host, root := testHost(t)
	writeStatusHost(t, root, serveAdminAPI(t, admin.Options{
		Upstreams:     &fakeUpstreams{},
		Store:         st,
		UserID:        "local",
		Config:        func() admin.ConfigState { return admin.ConfigState{} },
		CallRetention: func() time.Duration { return retention },
		Logger:        zerolog.New(zerolog.NewTestWriter(t)),
	}))

	return host
}

func TestCallsSendsItsFlagsAsTheQuery(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want store.CallFilter
		// since is how far back Since must be, or 0 for no bound.
		since time.Duration
	}{
		{"defaults", nil, store.CallFilter{Limit: 50}, 24 * time.Hour},
		{"every flag", []string{"--connector", "linear", "--connector", "slack", "--since", "2h", "--errors", "--limit", "7"},
			store.CallFilter{Connectors: []string{"linear", "slack"}, ErrorsOnly: true, Limit: 7}, 2 * time.Hour},
		{"whole log", []string{"--since", "0"}, store.CallFilter{Limit: 50}, 0},
		{"days", []string{"--since", "7d"}, store.CallFilter{Limit: 50}, 7 * 24 * time.Hour},
		{"days and hours", []string{"--since=1d12h"}, store.CallFilter{Limit: 50}, 36 * time.Hour},
		{"minutes", []string{"--since", "90m"}, store.CallFilter{Limit: 50}, 90 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{}
			host := callsDaemon(t, st, time.Hour)
			var stdout, stderr bytes.Buffer
			before := time.Now()

			if code := RunWith(append([]string{"calls"}, tt.args...), &stdout, &stderr, host); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			got := st.lastFilter()
			gotSince := got.Since
			got.Since = time.Time{}
			want := tt.want
			want.UserID = "local"

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("filter (-want +got):\n%s", diff)
			}

			if tt.since == 0 {
				if !gotSince.IsZero() {
					t.Errorf("Since = %v, want no bound", gotSince)
				}

				return
			}

			if lo, hi := before.Add(-tt.since-time.Second), time.Now().Add(-tt.since); gotSince.Before(lo) || gotSince.After(hi) {
				t.Errorf("Since = %v, want %v before now", gotSince, tt.since)
			}
		})
	}
}

func TestCallsPrintsTheTableAndJSON(t *testing.T) {
	at := time.Date(2026, 11, 8, 9, 14, 2, 0, time.UTC)
	ms := 412 * time.Millisecond
	st := &fakeStore{calls: []store.Call{{Client: "claude-code", TokenName: "default", Connector: "linear", Tool: "create_issue",
		StartedAt: at, Duration: &ms, Result: store.CallOK}}}
	host := callsDaemon(t, st, 720*time.Hour)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"calls"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want := "TIME                  CLIENT       TOKEN    TOOL                 DURATION  RESULT\n" +
		"2026-11-08T09:14:02Z  claude-code  default  linear.create_issue  412ms     ok\n"

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	stdout.Reset()

	if code := RunWith([]string{"calls", "--json"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("--json: exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	var got admin.Calls

	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a Calls: %v\n%s", err, stdout.String())
	}

	if got.Retention != "720h0m0s" || len(got.Calls) != 1 || got.Calls[0].Tool != "create_issue" || got.Calls[0].TokenName != "default" {
		t.Errorf("--json = %+v, want the retention and the one call, with its token", got)
	}
}

func TestCallsSaysWhenTheLogIsOff(t *testing.T) {
	host := callsDaemon(t, &fakeStore{}, 0)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"calls"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want := "No calls match.\n\nThe call log is off: [calls] retention is 0s, so no calls are recorded.\n"

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

func TestCallsRefusesBadFlagsBeforeAskingTheDaemon(t *testing.T) {
	// No daemon: a flag error must come first.
	host, _ := testHost(t)

	for _, args := range [][]string{
		{"--limit", "0"},
		{"--limit", "1001"},
		{"--since", "-1h"},
		{"--since", "-1d"},
		{"--since", "garbage"},
		{"--since", "7days"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			if code := RunWith(append([]string{"calls"}, args...), &stdout, &stderr, host); code == 0 {
				t.Fatalf("exit code = 0, want non-zero; stdout: %s", stdout.String())
			}

			if !strings.Contains(stderr.String(), args[0]) {
				t.Errorf("stderr = %q, want it to name %s", stderr.String(), args[0])
			}
		})
	}
}

// parseLookback is the --since grammar, a decision table tested directly
// (CODING_STANDARDS.md, Tests); TestCallsSendsItsFlagsAsTheQuery and
// TestCallsRefusesBadFlagsBeforeAskingTheDaemon cover it through the
// command.
func TestParseLookback(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want time.Duration
	}{
		{"0", 0},
		{"90m", 90 * time.Minute},
		{"36h", 36 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"+2h", 2 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"0d", 0},
		{"1d12h", 36 * time.Hour},
		{"2d30m15s", 48*time.Hour + 30*time.Minute + 15*time.Second},
		{"-1h", -time.Hour},
		{"-1d", -24 * time.Hour},
		{"-1d12h", -36 * time.Hour},
	} {
		got, err := parseLookback(tt.in)

		if err != nil || got != tt.want {
			t.Errorf("parseLookback(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}

	for _, in := range []string{
		"", "-", "d", "7", "garbage", "7days", "1.5d", "12h1d", "1d-2h", "1d+2h", "d12h", "+-1d", "--1h",
		"999999999999d", "106751d23h59m59s",
	} {
		if got, err := parseLookback(in); err == nil {
			t.Errorf("parseLookback(%q) = %v, want an error", in, got)
		}
	}
}
