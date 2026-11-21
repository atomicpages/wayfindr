package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

const authorizeURL = "https://as.example/authorize?client_id=x&state=s"

// scriptedFlows is the daemon's side of one auth flow: StartFlow hands out
// flow f1, and WaitFlow reports each of reports in turn, the last one for
// good.
type scriptedFlows struct {
	mu      sync.Mutex
	err     error
	reports []admin.FlowReport
	waits   int
}

func (f *scriptedFlows) StartFlow(_ context.Context, connector string, start admin.AuthStart) (admin.AuthFlow, error) {
	if f.err != nil {
		return admin.AuthFlow{}, f.err
	}

	expires := time.Date(2099, 1, 1, 0, 10, 0, 0, time.UTC)

	if start.Device {
		return admin.AuthFlow{FlowID: "f1", Connector: connector, UserCode: "WDJB-MJHT",
			VerificationURI: "https://as.example/device", VerificationURIComplete: "https://as.example/device?user_code=WDJB-MJHT",
			ExpiresAt: expires}, nil
	}

	return admin.AuthFlow{FlowID: "f1", Connector: connector, AuthorizeURL: authorizeURL, ExpiresAt: expires}, nil
}

func (f *scriptedFlows) WaitFlow(_ context.Context, id string) (admin.FlowReport, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if id != "f1" {
		return admin.FlowReport{}, false
	}

	rep := f.reports[min(f.waits, len(f.reports)-1)]
	f.waits++

	return rep, true
}

// waitCount returns how many long-polls the flow has had.
func (f *scriptedFlows) waitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.waits
}

// authHost serves the admin API over flows, with linear's catalog holding
// three tools, and returns a host whose browser opener records the URLs
// it was asked to open, failing with openErr.
func authHost(t *testing.T, flows admin.Flows, openErr error) (Host, *[]string) {
	t.Helper()
	addr := serveAdminAPI(t, admin.Options{
		Upstreams: &fakeUpstreams{},
		Store:     &fakeStore{tools: map[string]int{"linear": 3}},
		UserID:    "local",
		Config:    func() admin.ConfigState { return admin.ConfigState{} },
		Flows:     flows,
		Logger:    zerolog.New(zerolog.NewTestWriter(t)),
	})
	host, root := testHost(t)
	writeStatusHost(t, root, addr)
	var opened []string
	host.OpenURL = func(_ context.Context, u string) error {
		opened = append(opened, u)

		return openErr
	}

	return host, &opened
}

const authPrompt = `Open this URL to authorize linear:
  https://as.example/authorize?client_id=x&state=s
Waiting for the browser (until 2099-01-01T00:10:00Z). If the page shows an error, fix it and run ` + "`wayfindr auth login linear`" + `; linear stays configured.
`

func TestAuthOpensTheURLAndWaitsUntilTheConnectorIsReady(t *testing.T) {
	flows := &scriptedFlows{reports: []admin.FlowReport{
		{Connector: "linear", State: admin.FlowPending},
		{Connector: "linear", State: admin.FlowDone, Status: &upstream.ConnectorStatus{Connector: "linear", State: upstream.StateReady}},
	}}
	host, opened := authHost(t, flows, nil)
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"auth", "login", "linear"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want := authPrompt + "Authorized linear.\nlinear is ready (3 tools).\n"

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff([]string{authorizeURL}, *opened); diff != "" {
		t.Errorf("opened (-want +got):\n%s", diff)
	}

	if flows.waits != 2 {
		t.Errorf("long-polls = %d, want 2: it polls again while pending", flows.waits)
	}
}

// The bare `auth <connector>` is the old spelling of `auth login`, kept
// hidden, and a connector named after a subcommand is still reachable.
func TestAuthLoginAndTheOldBareFormBothAuthorize(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		connector string
		opened    []string
	}{
		{"login", []string{"auth", "login", "linear"}, "linear", []string{authorizeURL}},
		{"bare", []string{"auth", "linear"}, "linear", []string{authorizeURL}},
		{"bare with a flag", []string{"auth", "--no-open", "linear"}, "linear", nil},
		{"login of a connector named revoke", []string{"auth", "login", "revoke"}, "revoke", []string{authorizeURL}},
		{"bare after --", []string{"auth", "--", "revoke"}, "revoke", []string{authorizeURL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flows := &scriptedFlows{reports: []admin.FlowReport{
				{Connector: tc.connector, State: admin.FlowDone, Status: &upstream.ConnectorStatus{Connector: tc.connector, State: upstream.StateReady}},
			}}
			host, opened := authHost(t, flows, nil)
			var stdout, stderr bytes.Buffer

			if code := RunWith(tc.args, &stdout, &stderr, host); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			if want := "Open this URL to authorize " + tc.connector + ":\n"; !strings.HasPrefix(stdout.String(), want) {
				t.Errorf("stdout = %q, want it to start with %q", stdout.String(), want)
			}

			if diff := cmp.Diff(tc.opened, *opened); diff != "" {
				t.Errorf("opened (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAuthWithoutAnArgumentPrintsTheGroupsHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := Run([]string{"auth"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	help := stdout.String()

	for _, want := range []string{"\n  login ", "\n  logout "} {
		if !strings.Contains(help, want) {
			t.Errorf("help = %q, want it to list %q", help, strings.TrimSpace(want))
		}
	}

	if strings.Contains(help, "revoke") {
		t.Errorf("help = %q, want the old revoke hidden", help)
	}
}

func TestAuthDevicePrintsTheCodeAndOpensTheVerificationURI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		opened []string
	}{
		{"opens the complete URI", []string{"auth", "login", "linear", "--device"}, []string{"https://as.example/device?user_code=WDJB-MJHT"}},
		{"--no-open only prints", []string{"auth", "login", "linear", "--device", "--no-open"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flows := &scriptedFlows{reports: []admin.FlowReport{
				{Connector: "linear", State: admin.FlowPending},
				{Connector: "linear", State: admin.FlowDone, Status: &upstream.ConnectorStatus{Connector: "linear", State: upstream.StateReady}},
			}}
			host, opened := authHost(t, flows, nil)
			var stdout, stderr bytes.Buffer

			if code := RunWith(tc.args, &stdout, &stderr, host); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			want := "Enter code WDJB-MJHT at https://as.example/device to authorize linear.\n" +
				"Waiting for you to allow access (until 2099-01-01T00:10:00Z). If the page shows an error, fix it and run " +
				"`wayfindr auth login linear --device`; linear stays configured.\n" +
				"Authorized linear.\nlinear is ready (3 tools).\n"

			if diff := cmp.Diff(want, stdout.String()); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff(tc.opened, *opened); diff != "" {
				t.Errorf("opened (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAuthReportsAConnectorThatIsNotReadyAfterIt(t *testing.T) {
	flows := &scriptedFlows{reports: []admin.FlowReport{{Connector: "linear", State: admin.FlowDone,
		Status: &upstream.ConnectorStatus{Connector: "linear", State: upstream.StateUnreachable,
			Kind: upstream.KindUnreachable}}}}
	host, _ := authHost(t, flows, nil)
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"auth", "login", "linear"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0: the authorization succeeded; stderr: %s", code, stderr.String())
	}

	if out := stdout.String(); !strings.HasPrefix(out, authPrompt+"Authorized linear.\nlinear: ") {
		t.Errorf("stdout = %q, want the authorization and linear's advice", out)
	}
}

func TestAuthThatFailsShowsTheAdviceAndExitsNonZero(t *testing.T) {
	flows := &scriptedFlows{reports: []admin.FlowReport{{Connector: "linear", State: admin.FlowFailed, Advice: &admin.Advice{
		Summary: "The authorization was not granted (provider code access_denied).",
		Action:  "Open the link to try again, and allow access on the provider's page.",
		Link:    "http://127.0.0.1:7777/auth/linear",
	}}}}
	host, _ := authHost(t, flows, nil)
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"auth", "login", "linear"}, &stdout, &stderr, host)

	if code == 0 {
		t.Fatal("exit code = 0, want non-zero")
	}

	want := authPrompt + "linear was not authorized. Fix the cause, then run `wayfindr auth login linear`.\n" + `linear: The authorization was not granted (provider code access_denied).
  Open the link to try again, and allow access on the provider's page.
  http://127.0.0.1:7777/auth/linear
`

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff("Error: the authorization of linear failed\n", stderr.String()); diff != "" {
		t.Errorf("stderr (-want +got):\n%s", diff)
	}
}

func TestAuthWithoutABrowserStillWaits(t *testing.T) {
	flows := &scriptedFlows{reports: []admin.FlowReport{{Connector: "linear", State: admin.FlowDone,
		Status: &upstream.ConnectorStatus{Connector: "linear", State: upstream.StateReady}}}}
	host, _ := authHost(t, flows, errors.New("xdg-open: not found"))
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"auth", "login", "linear"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stderr.String(), "Could not open a browser (xdg-open: not found); open the URL above yourself.") {
		t.Errorf("stderr = %q, want the hint to open the URL by hand", stderr.String())
	}
}

func TestAuthNoOpenPrintsTheURLAndNeverOpensABrowser(t *testing.T) {
	flows := &scriptedFlows{reports: []admin.FlowReport{{Connector: "linear", State: admin.FlowDone,
		Status: &upstream.ConnectorStatus{Connector: "linear", State: upstream.StateReady}}}}
	host, opened := authHost(t, flows, nil)
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"auth", "login", "--no-open", "linear"}, &stdout, &stderr, host)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want := authPrompt + "Authorized linear.\nlinear is ready (3 tools).\n"

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	if len(*opened) != 0 {
		t.Errorf("opened %v, want nothing", *opened)
	}

	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

func TestAuthOfAConnectorTheDaemonRefusesSaysWhy(t *testing.T) {
	flows := &scriptedFlows{err: &admin.FlowError{Status: http.StatusNotFound, Kind: "request", Case: "unknown_connector",
		Advice: admin.Advice{
			Summary: `No connector named "nope" is configured.`, Action: "Check the connector's name with `wayfindr status`.",
		}}}
	host, opened := authHost(t, flows, nil)
	var stdout, stderr bytes.Buffer

	code := RunWith([]string{"auth", "login", "nope"}, &stdout, &stderr, host)

	if code == 0 {
		t.Fatal("exit code = 0, want non-zero")
	}

	if !strings.Contains(stderr.String(), "No connector named \"nope\" is configured.\nCheck the connector's name with `wayfindr status`.") {
		t.Errorf("stderr = %q, want auth's advice", stderr.String())
	}

	if len(*opened) != 0 {
		t.Errorf("opened %v, want nothing", *opened)
	}
}

func TestOpenBrowserRefusesNonHTTPURLs(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "javascript:alert(1)", "calc.exe"} {
		if err := openBrowser(t.Context(), u); err == nil {
			t.Errorf("openBrowser(%q) = nil, want a refusal", u)
		}
	}
}
