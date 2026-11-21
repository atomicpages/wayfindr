package daemon_test

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.uber.org/goleak"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
	"github.com/atomicpages/wayfindr/internal/testutil/testcert"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// A refresh token the provider says ends within a day is a warning every
// reader sees: the user in the status, with the link to authorize again;
// the agent in search_tools, so it can say so before a call fails; and the
// operator in the log, once, at the next check.
func TestARefreshTokenEndingSoonIsWarnedOfInStatusSearchAndTheLog(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	proxy := reverseProxy(t, fakeupstream.StartStreamable(t, fakeupstream.V20250618).URL)
	as := fakeas.Start(t, fakeas.WithRefreshTokenLifetime(12*time.Hour), fakeas.WithResourceHandler(proxy))
	clock := newFakeClock()
	logs := &syncBuffer{}
	h := newHost(t)
	opts := pollOptions(t, h, clock)
	opts.Logger = zerolog.New(io.MultiWriter(logs, zerolog.NewTestWriter(t))).Level(zerolog.InfoLevel)
	r := serve(t, h, opts, oauthConfig(freeAddr(t), as.ResourceURL))
	r.waitStatus(t, "linear needs_auth", stateIs("linear", upstream.StateNeedsAuth))
	// The startup check has run: it arms the next one.
	clock.waitArmed(t, epoch.Add(15*time.Minute))
	authorizeLinear(t, r)

	c, _ := connector(getStatus(t, r.client, "http://"+r.addr), "linear")

	if len(c.Warnings) != 1 || c.Warnings[0].Code != "refresh_expiring" || c.Warnings[0].Note ||
		c.Warnings[0].Link != "http://"+r.addr+"/auth/linear" {
		t.Fatalf("linear's warnings = %+v, want one refresh_expiring warning with the auth link", c.Warnings)
	}

	var page struct {
		Warnings []admin.Advice `json:"warnings"`
	}

	if err := json.Unmarshal([]byte(call(t, r.mcp, "search_tools", map[string]any{"query": "anything"})), &page); err != nil {
		t.Fatalf("search_tools response: %v", err)
	}

	if want := c.Warnings[0].Advice; len(page.Warnings) != 1 || page.Warnings[0] != want {
		t.Errorf("search_tools warnings = %+v, want linear's %+v", page.Warnings, want)
	}

	// Each check arms the next one once it has logged.
	for i, at := range []time.Duration{30 * time.Minute, 45 * time.Minute} {
		clock.advance(15 * time.Minute)
		clock.waitArmed(t, epoch.Add(at))

		if n := strings.Count(logs.String(), `"warning":"refresh_expiring"`); n != 1 {
			t.Fatalf("after check %d, refresh_expiring logged %d times, want once; log:\n%s", i+1, n, logs.String())
		}
	}
}

// A url connector's client certificate that expires within a day is a
// warning every reader sees, OAuth or not: the status, search_tools, and
// the log (certificate_expiring).
func TestAnExpiringClientCertificateIsWarnedOf(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	up := fakeupstream.StartStreamable(t, fakeupstream.V20250618)
	ca := testcert.NewCA(t)
	dir := t.TempDir()
	certPEM, keyPEM := ca.IssueUntil(t, "client-1", false, time.Now().Add(12*time.Hour))
	clock := newFakeClock()
	logs := &syncBuffer{}
	h := newHost(t)
	opts := pollOptions(t, h, clock)
	opts.Logger = zerolog.New(io.MultiWriter(logs, zerolog.NewTestWriter(t))).Level(zerolog.InfoLevel)
	r := serve(t, h, opts, map[string]any{
		"server": map[string]any{"listen": freeAddr(t)},
		"connectors": map[string]any{"files": map[string]any{
			"url": up.URL,
			"tls": map[string]any{
				"cert_file": testcert.Write(t, dir, "c.pem", certPEM),
				"key_file":  testcert.Write(t, dir, "c.key", keyPEM),
			},
		}},
	})
	r.waitStatus(t, "files ready", stateIs("files", upstream.StateReady))
	clock.waitArmed(t, epoch.Add(15*time.Minute))

	c, _ := connector(getStatus(t, r.client, "http://"+r.addr), "files")

	if len(c.Warnings) != 1 || c.Warnings[0].Code != "certificate_expiring" || c.Warnings[0].Note ||
		!strings.Contains(c.Warnings[0].Summary, "[connectors.files.tls]") {
		t.Fatalf("files' warnings = %+v, want one certificate_expiring warning", c.Warnings)
	}

	var page struct {
		Warnings []admin.Advice `json:"warnings"`
	}

	if err := json.Unmarshal([]byte(call(t, r.mcp, "search_tools", map[string]any{"query": "anything"})), &page); err != nil {
		t.Fatalf("search_tools response: %v", err)
	}

	if want := c.Warnings[0].Advice; len(page.Warnings) != 1 || page.Warnings[0] != want {
		t.Errorf("search_tools warnings = %+v, want files' %+v", page.Warnings, want)
	}

	if n := strings.Count(logs.String(), `"warning":"certificate_expiring"`); n != 1 {
		t.Errorf("certificate_expiring logged %d times, want once; log:\n%s", n, logs.String())
	}
}
