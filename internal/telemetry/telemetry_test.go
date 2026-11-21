package telemetry_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/telemetry"
)

const (
	goldenID = "6f1c2b7e-3d4a-4c1e-9b2f-8a7d5e0c1f23"

	// goldenConfig has one connector of each transport and auth kind, an
	// OpenAI embedding, and two profiles: the config report-v1.json
	// describes.
	goldenConfig = `
[embedding]
base_url   = "https://api.openai.com/v1"
model      = "text-embedding-3-small"
dimensions = 1536
api_key    = "${env:OPENAI_API_KEY}"

[connectors.fs]
command = "npx"
args    = ["-y", "@modelcontextprotocol/server-filesystem", "/srv"]

[connectors.linear]
url  = "https://mcp.linear.app/mcp"
auth = { type = "oauth" }

[connectors.legacy]
url       = "https://legacy.example.com/sse"
transport = "sse"
headers   = { Authorization = "Bearer ${env:LEGACY_TOKEN}" }

[connectors.open]
url = "https://open.example.com/mcp"

[profiles.work]
connectors = ["linear"]

[profiles.home]
connectors = ["fs"]
`
)

// collector is a fake collector: it records every request and answers
// with status.
type collector struct {
	mu       sync.Mutex
	status   int
	requests []received
}

type received struct {
	method, path, contentType, userAgent string
	body                                 []byte
}

func newCollector(t *testing.T) (*collector, *httptest.Server) {
	t.Helper()
	c := &collector{status: http.StatusNoContent}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.requests = append(c.requests, received{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			userAgent:   r.Header.Get("User-Agent"),
			body:        body,
		})
		w.WriteHeader(c.status)
	}))
	t.Cleanup(srv.Close)

	return c, srv
}

func (c *collector) answer(status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = status
}

func (c *collector) received() []received {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]received(nil), c.requests...)
}

// last decodes the last report received.
func (c *collector) last(t *testing.T) telemetry.Report {
	t.Helper()
	reqs := c.received()

	if len(reqs) == 0 {
		t.Fatal("collector received no report")
	}

	var r telemetry.Report

	if err := json.Unmarshal(reqs[len(reqs)-1].body, &r); err != nil {
		t.Fatalf("decode report: %v", err)
	}

	return r
}

func parseConfig(t *testing.T, toml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(toml), config.Options{DataDir: t.TempDir()})

	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	return cfg
}

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// harness is a Reporter for a release build, sending to a fake collector.
type harness struct {
	reporter  *telemetry.Reporter
	collector *collector
	clock     *clock
	idFile    string
}

type tweak func(*telemetry.Options)

func newHarness(t *testing.T, cfg *config.Config, tweaks ...tweak) harness {
	t.Helper()
	col, srv := newCollector(t)
	clk := &clock{now: time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)}
	idFile := filepath.Join(t.TempDir(), "telemetry-id")
	opts := telemetry.Options{
		Endpoint: srv.URL,
		IDFile:   idFile,
		Version:  "v0.4.0",
		Commit:   "2e3a922",
		Config:   func() *config.Config { return cfg },
		Now:      clk.Now,
		// Low, so tests solve fast; TestReportMatchesGolden uses the real one.
		Difficulty: 4,
		Logger:     zerolog.Nop(),
	}

	for _, f := range tweaks {
		f(&opts)
	}

	return harness{reporter: telemetry.New(opts), collector: col, clock: clk, idFile: idFile}
}

func (h harness) send(t *testing.T) error {
	t.Helper()

	return h.reporter.Send(context.Background())
}

func TestReportMatchesGolden(t *testing.T) {
	h := newHarness(t, parseConfig(t, goldenConfig), func(o *telemetry.Options) { o.Difficulty = 0 })

	if err := os.WriteFile(h.idFile, []byte(goldenID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := h.reporter
	r.SetEnabled(true)

	for range 3 {
		r.Search()
	}

	r.Call("claude-code", store.CallOK)
	r.Call("claude-code", store.CallOK)
	r.Call("Cursor-VSCode", store.CallToolError)
	r.Call("my-private-agent", store.CallFailed)
	r.Call("", store.CallRejected)
	// 24.5 hours after telemetry was turned on: window_hours is 25.
	h.clock.set(time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC))

	if err := h.send(t); err != nil {
		t.Fatalf("Send: %v", err)
	}

	reqs := h.collector.received()

	if len(reqs) != 1 {
		t.Fatalf("collector received %d requests, want 1", len(reqs))
	}

	req := reqs[0]
	gotHead := []string{req.method, req.path, req.contentType, req.userAgent}
	wantHead := []string{http.MethodPost, "/v1/report", "application/json", "wayfindr/v0.4.0"}

	if diff := cmp.Diff(wantHead, gotHead); diff != "" {
		t.Errorf("request (-want +got):\n%s", diff)
	}

	var got, want map[string]any

	if err := json.Unmarshal(req.body, &got); err != nil {
		t.Fatalf("decode report: %v", err)
	}

	golden, err := os.ReadFile(filepath.Join("testdata", "report-v1.json"))

	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(golden, &want); err != nil {
		t.Fatalf("decode golden: %v", err)
	}

	// The golden file holds an example build platform; the report carries
	// the one the test runs on.
	want["go"], _, _ = strings.Cut(runtime.Version(), " ")
	want["os"] = runtime.GOOS
	want["arch"] = runtime.GOARCH

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("report (-golden +got):\n%s", diff)
	}
}

type powVectors struct {
	Difficulty int `json:"difficulty"`
	Vectors    []struct {
		InstallID string `json:"install_id"`
		Date      string `json:"date"`
		Nonce     string `json:"nonce"`
		SHA256    string `json:"sha256"`
		Valid     bool   `json:"valid"`
	} `json:"vectors"`
}

func TestProofOfWorkVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "pow-v1.json"))

	if err != nil {
		t.Fatal(err)
	}

	var pv powVectors

	if err := json.Unmarshal(data, &pv); err != nil {
		t.Fatal(err)
	}

	if pv.Difficulty != telemetry.Difficulty {
		t.Fatalf("vectors are for difficulty %d, schema 1's is %d", pv.Difficulty, telemetry.Difficulty)
	}

	for _, v := range pv.Vectors {
		t.Run(v.InstallID+"/"+v.Date+"/"+v.Nonce, func(t *testing.T) {
			sum := sha256.Sum256([]byte("wayfindr-report-v1|" + v.InstallID + "|" + v.Date + "|" + v.Nonce))

			if got := hex.EncodeToString(sum[:]); got != v.SHA256 {
				t.Errorf("sha256 = %s, vector says %s", got, v.SHA256)
			}

			if got := telemetry.Verify(v.InstallID, v.Date, v.Nonce, pv.Difficulty); got != v.Valid {
				t.Errorf("Verify = %t, want %t", got, v.Valid)
			}

			if !v.Valid {
				return
			}

			nonce, err := telemetry.Solve(context.Background(), v.InstallID, v.Date, pv.Difficulty)

			if err != nil {
				t.Fatalf("Solve: %v", err)
			}

			// Solve counts up from zero, so it finds the vector's nonce or
			// a smaller valid one; the vectors hold the first.
			if nonce != v.Nonce {
				t.Errorf("Solve = %s, want %s", nonce, v.Nonce)
			}
		})
	}
}

func TestSolveStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := telemetry.Solve(ctx, goldenID, "2026-11-18", 64); !errors.Is(err, context.Canceled) {
		t.Errorf("Solve with a cancelled ctx = %v, want context.Canceled", err)
	}
}

func TestNothingSentWhenOff(t *testing.T) {
	cfg := parseConfig(t, goldenConfig)

	for _, tc := range []struct {
		name    string
		enabled bool
		tweak   tweak
	}{
		{name: "config off", enabled: false, tweak: func(*telemetry.Options) {}},
		{name: "DO_NOT_TRACK", enabled: true, tweak: func(o *telemetry.Options) { o.DoNotTrack = true }},
		{name: "dev version", enabled: true, tweak: func(o *telemetry.Options) { o.Version = "dev" }},
		{name: "git describe version", enabled: true, tweak: func(o *telemetry.Options) { o.Version = "v0.4.0-3-g2e3a922-dirty" }},
		{name: "dirty tag build", enabled: true, tweak: func(o *telemetry.Options) { o.Version = "v0.4.0-dirty" }},
		{name: "unknown commit", enabled: true, tweak: func(o *telemetry.Options) { o.Commit = "unknown" }},
		{name: "no endpoint", enabled: true, tweak: func(o *telemetry.Options) { o.Endpoint = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, cfg, tc.tweak)
			h.reporter.SetEnabled(tc.enabled)
			h.reporter.Search()
			h.reporter.Call("claude-code", store.CallOK)

			if err := h.send(t); err != nil {
				t.Fatalf("Send: %v", err)
			}

			if n := len(h.collector.received()); n != 0 {
				t.Errorf("collector received %d reports, want none", n)
			}

			if _, err := os.Stat(h.idFile); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("install ID file: Stat = %v, want it not to exist", err)
			}
		})
	}
}

func TestDoNotTrack(t *testing.T) {
	for value, want := range map[string]bool{"": false, "0": false, "1": true, "true": true, "yes": true} {
		if got := telemetry.DoNotTrack(value); got != want {
			t.Errorf("DoNotTrack(%q) = %t, want %t", value, got, want)
		}
	}
}

func TestCountsWaitForAnAcceptedReport(t *testing.T) {
	h := newHarness(t, parseConfig(t, goldenConfig))
	r := h.reporter
	start := h.clock.Now()
	r.SetEnabled(true)
	r.Search()
	r.Call("codex-mcp-client", store.CallOK)

	h.collector.answer(http.StatusInternalServerError)
	h.clock.set(start.Add(24 * time.Hour))

	if err := h.send(t); err == nil {
		t.Fatal("Send with the collector answering 500 = nil, want an error")
	}

	r.Search()
	h.collector.answer(http.StatusNoContent)
	h.clock.set(start.Add(48 * time.Hour))

	if err := h.send(t); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := h.collector.last(t)
	want := usageOf(48, 2, telemetry.Calls{OK: 1}, map[string]int64{"codex": 1})

	if diff := cmp.Diff(want, usageOf(got.WindowHours, got.Searches, got.Calls, got.Clients)); diff != "" {
		t.Errorf("report after a failed one (-want +got):\n%s", diff)
	}

	// Accepted: the next report starts from nothing, and from then.
	h.clock.set(start.Add(60 * time.Hour))

	if err := h.send(t); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got = h.collector.last(t)
	want = usageOf(12, 0, telemetry.Calls{}, map[string]int64{})

	if diff := cmp.Diff(want, usageOf(got.WindowHours, got.Searches, got.Calls, got.Clients)); diff != "" {
		t.Errorf("report after an accepted one (-want +got):\n%s", diff)
	}
}

func TestTurningOffDropsCounts(t *testing.T) {
	h := newHarness(t, parseConfig(t, goldenConfig))
	r := h.reporter
	r.SetEnabled(true)
	r.Search()
	r.Call("cursor", store.CallOK)
	r.SetEnabled(false)
	// Not counted: telemetry is off.
	r.Search()
	h.clock.set(h.clock.Now().Add(time.Hour))
	r.SetEnabled(true)
	h.clock.set(h.clock.Now().Add(2 * time.Hour))

	if err := h.send(t); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := h.collector.last(t)
	want := usageOf(2, 0, telemetry.Calls{}, map[string]int64{})

	if diff := cmp.Diff(want, usageOf(got.WindowHours, got.Searches, got.Calls, got.Clients)); diff != "" {
		t.Errorf("report (-want +got):\n%s", diff)
	}
}

type usage struct {
	WindowHours int
	Searches    int64
	Calls       telemetry.Calls
	Clients     map[string]int64
}

func usageOf(window int, searches int64, calls telemetry.Calls, clients map[string]int64) usage {
	return usage{WindowHours: window, Searches: searches, Calls: calls, Clients: clients}
}

func TestInstallID(t *testing.T) {
	h := newHarness(t, parseConfig(t, goldenConfig))
	h.reporter.SetEnabled(true)

	ids := func() string {
		t.Helper()

		if err := h.send(t); err != nil {
			t.Fatalf("Send: %v", err)
		}

		return h.collector.last(t).InstallID
	}

	first := ids()

	if u, err := uuid.Parse(first); err != nil || u.Version() != 4 || u.String() != first {
		t.Fatalf("install ID %q is not a lowercase UUIDv4", first)
	}

	if again := ids(); again != first {
		t.Errorf("second report's install ID = %s, want the first's %s", again, first)
	}

	if err := os.Remove(h.idFile); err != nil {
		t.Fatal(err)
	}

	if fresh := ids(); fresh == first {
		t.Error("install ID unchanged after its file was deleted")
	}

	if err := os.WriteFile(h.idFile, []byte("not an id\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	replaced := ids()

	if _, err := uuid.Parse(replaced); err != nil {
		t.Errorf("install ID %q after a garbled file is not a UUID", replaced)
	}
}

func TestNamesOutsideAllowlistsAreOther(t *testing.T) {
	for _, tc := range []struct {
		name               string
		baseURL, model     string
		provider, modelKey string
	}{
		{"openai", "https://api.openai.com/v1", "text-embedding-3-large", "openai", "text-embedding-3-large"},
		{"ollama tag", "http://127.0.0.1:11434/v1", "qwen3-embedding:0.6b", "local", "qwen3-embedding"},
		{"org prefix", "http://localhost:8080/v1", "BAAI/bge-m3", "local", "bge-m3"},
		{"private address", "http://10.0.0.5/v1", "nomic-embed-text", "local", "nomic-embed-text"},
		{"own host", "https://embed.internal.acme.com/v1", "acme-legal-embed-v2", "other", "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parseConfig(t, `
[embedding]
base_url   = "`+tc.baseURL+`"
model      = "`+tc.model+`"
dimensions = 1024
`)
			h := newHarness(t, cfg)
			h.reporter.SetEnabled(true)

			if err := h.send(t); err != nil {
				t.Fatalf("Send: %v", err)
			}

			got := h.collector.last(t)

			if diff := cmp.Diff([]string{tc.provider, tc.modelKey}, []string{got.EmbeddingProvider, got.EmbeddingModel}); diff != "" {
				t.Errorf("embedding provider, model (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNoEmbeddingLeavesItsNamesOut(t *testing.T) {
	h := newHarness(t, parseConfig(t, ""))
	h.reporter.SetEnabled(true)

	if err := h.send(t); err != nil {
		t.Fatalf("Send: %v", err)
	}

	body := string(h.collector.received()[0].body)

	for _, key := range []string{`"embedding_provider"`, `"embedding_model"`} {
		if strings.Contains(body, key) {
			t.Errorf("report without [embedding] has %s: %s", key, body)
		}
	}

	if !strings.Contains(body, `"clients":{}`) {
		t.Errorf("report with no calls has no empty clients object: %s", body)
	}
}

// TestTurningOffDuringASendSendsNothing: a send that started before
// telemetry was turned off doesn't post once it has solved (ADR 0020).
func TestTurningOffDuringASendSendsNothing(t *testing.T) {
	cfg := parseConfig(t, goldenConfig)
	var r *telemetry.Reporter
	h := newHarness(t, cfg, func(o *telemetry.Options) {
		// Config is read once the send has begun: turn telemetry off then.
		o.Config = func() *config.Config {
			r.SetEnabled(false)

			return cfg
		}
	})
	r = h.reporter
	r.SetEnabled(true)
	r.Call("claude-code", store.CallOK)

	if err := h.send(t); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if n := len(h.collector.received()); n != 0 {
		t.Errorf("collector received %d reports, want none after telemetry was turned off", n)
	}
}

// TestRedirectIsAFailedSend: the report never follows a redirect, so its
// body goes nowhere but the collector.
func TestRedirectIsAFailedSend(t *testing.T) {
	elsewhere, other := newCollector(t)
	redirect := httptest.NewServer(http.RedirectHandler(other.URL+"/v1/report", http.StatusTemporaryRedirect))
	t.Cleanup(redirect.Close)
	h := newHarness(t, parseConfig(t, goldenConfig), func(o *telemetry.Options) { o.Endpoint = redirect.URL })
	h.reporter.SetEnabled(true)

	if err := h.send(t); err == nil {
		t.Error("Send answered with a redirect = nil, want an error")
	}

	if n := len(elsewhere.received()); n != 0 {
		t.Errorf("redirect target received %d reports, want none", n)
	}
}

// TestSendErrorLeavesTheEndpointOut: a failed send's error, which the
// daemon logs, doesn't carry the endpoint URL or its query.
func TestSendErrorLeavesTheEndpointOut(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL + "/?key=hunter2"
	srv.Close()
	h := newHarness(t, parseConfig(t, goldenConfig), func(o *telemetry.Options) { o.Endpoint = endpoint })
	h.reporter.SetEnabled(true)
	err := h.send(t)

	if err == nil {
		t.Fatal("Send to a closed server = nil, want an error")
	}

	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error %q names the endpoint", err)
	}
}
