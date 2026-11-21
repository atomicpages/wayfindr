package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// uiHost serves the admin API with a console that issues the login path
// "/ui/login#code", and returns a host whose browser opener records the
// URLs it was asked to open, failing with openErr, and the daemon's
// host:port.
func uiHost(t *testing.T, console bool, openErr error) (Host, *[]string, string) {
	t.Helper()
	opts := admin.Options{
		Upstreams: &fakeUpstreams{},
		Store:     &fakeStore{},
		UserID:    "local",
		Config:    func() admin.ConfigState { return admin.ConfigState{} },
		Logger:    zerolog.New(zerolog.NewTestWriter(t)),
	}

	if console {
		opts.UILogin = func() admin.UILogin {
			return admin.UILogin{Path: "/ui/login#code", ExpiresAt: time.Date(2099, 1, 1, 0, 1, 0, 0, time.UTC)}
		}
	}

	addr := serveAdminAPI(t, opts)
	host, root := testHost(t)
	writeStatusHost(t, root, addr)
	var opened []string
	host.OpenURL = func(_ context.Context, u string) error {
		opened = append(opened, u)

		return openErr
	}

	return host, &opened, addr
}

func TestUIOpensASignedInLink(t *testing.T) {
	host, opened, addr := uiHost(t, true, nil)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"ui"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if diff := cmp.Diff([]string{"http://" + addr + "/ui/login#code"}, *opened); diff != "" {
		t.Errorf("opened (-want +got):\n%s", diff)
	}

	if want := "Opened the console at http://" + addr + "/ui/.\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestUINoOpenPrintsTheLink(t *testing.T) {
	host, opened, addr := uiHost(t, true, nil)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"ui", "--no-open"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	want := "Open this link to use the console. It works once, until 2099-01-01T00:01:00Z:\n  http://" + addr + "/ui/login#code\n"

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	if len(*opened) != 0 {
		t.Errorf("opened %v, want nothing", *opened)
	}
}

func TestUIPrintsTheLinkWhenNoBrowserOpens(t *testing.T) {
	host, _, addr := uiHost(t, true, errors.New("no opener"))
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"ui"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stderr.String(), "Could not open a browser (no opener).") || !strings.Contains(stderr.String(), "http://"+addr+"/ui/login#code") {
		t.Errorf("stderr = %q, want the reason and the link", stderr.String())
	}
}

func TestUIWithoutAConsole(t *testing.T) {
	host, opened, _ := uiHost(t, false, nil)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"ui"}, &stdout, &stderr, host); code == 0 {
		t.Fatal("exit code = 0, want a failure")
	}

	if !strings.Contains(stderr.String(), "the web console is not available") {
		t.Errorf("stderr = %q, want the daemon's reason", stderr.String())
	}

	if len(*opened) != 0 {
		t.Errorf("opened %v, want nothing", *opened)
	}
}
