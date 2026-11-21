package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
)

const (
	// Endpoint is the collector's base URL; the report goes to
	// Endpoint + "/v1/report". The collector is a Cloudflare Worker the
	// maintainers run (ADR 0020).
	Endpoint = "https://wayfindr-collector.wingmen-unit-7h.workers.dev"

	// EndpointEnv overrides Endpoint, for testing a collector.
	EndpointEnv = "WAYFINDR_TELEMETRY_ENDPOINT"
	// DoNotTrackEnv is the variable that, set to anything but "" or "0",
	// turns telemetry off whatever the config says.
	DoNotTrackEnv = "DO_NOT_TRACK"

	// sendTimeout bounds one report's request.
	sendTimeout = 10 * time.Second
	// maxResponse is how much of a response body is read before closing,
	// so the connection can be reused. The client never reads it otherwise.
	maxResponse = 4 << 10
)

// Options configures a Reporter.
type Options struct {
	// Endpoint is the collector's base URL. Empty sends nothing.
	Endpoint string
	// HTTPClient sends the report. Nil means a client with sendTimeout, on
	// the default transport, so HTTPS_PROXY applies.
	HTTPClient *http.Client
	// IDFile holds the install ID (paths.TelemetryIDFile).
	IDFile string
	// Version and Commit are the build's. Only a release build sends: a
	// version like v1.2.3 or v1.2.3-rc.1 and a hex commit.
	Version string
	Commit  string
	// DoNotTrack is set when DO_NOT_TRACK says so; it overrides the config.
	DoNotTrack bool
	// Config returns the running config, or nil before the first apply.
	Config func() *config.Config
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Difficulty is the proof-of-work's. Zero means Difficulty; tests use
	// less.
	Difficulty int
	Logger     zerolog.Logger
}

// Reporter counts usage while telemetry is on and sends the usage report.
// Its methods are safe for concurrent use.
type Reporter struct {
	endpoint   string
	client     *http.Client
	idFile     string
	version    string
	commit     string
	config     func() *config.Config
	now        func() time.Time
	difficulty int
	log        zerolog.Logger
	// blocked is why telemetry stays off whatever the config says; empty
	// when nothing blocks it.
	blocked string

	mu sync.Mutex
	// on is telemetry.enabled as the running config has it.
	on bool
	// generation counts the times counting restarted, so a send that
	// overlapped a restart doesn't subtract its counts from the new ones.
	generation uint64
	since      time.Time
	usage      usage
}

// usage is what a Reporter has counted since its window began.
type usage struct {
	searches int64
	calls    Calls
	clients  map[string]int64
}

// New returns a Reporter, off until SetEnabled turns it on.
func New(opts Options) *Reporter {
	client := opts.HTTPClient

	if client == nil {
		client = &http.Client{
			Timeout: sendTimeout,
			// The report goes to the collector and nowhere else: a redirect
			// is a failed send, never a second POST of the body elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}

	now := opts.Now

	if now == nil {
		now = time.Now
	}

	difficulty := opts.Difficulty

	if difficulty == 0 {
		difficulty = Difficulty
	}

	return &Reporter{
		endpoint:   strings.TrimSuffix(opts.Endpoint, "/"),
		client:     client,
		idFile:     opts.IDFile,
		version:    opts.Version,
		commit:     opts.Commit,
		config:     opts.Config,
		now:        now,
		difficulty: difficulty,
		log:        opts.Logger,
		blocked:    blockedBy(opts),
		usage:      usage{clients: map[string]int64{}},
	}
}

// DoNotTrack reports whether a DO_NOT_TRACK value turns telemetry off.
func DoNotTrack(value string) bool { return value != "" && value != "0" }

// releaseVersion matches a release's version: v1.2.3, or a pre-release such
// as v1.2.3-rc.1. git describe's versions from a checkout (v1.2.3-4-gabc,
// v1.2.3-dirty) are not releases; isRelease refuses -dirty, which this
// pattern alone would let through.
func releaseVersion() *regexp.Regexp {
	return regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)
}

// hexCommit matches a commit hash, short or full.
func hexCommit() *regexp.Regexp { return regexp.MustCompile(`^[0-9a-f]{7,40}$`) }

// isRelease reports whether version and commit are a release build's: only
// those send the usage report.
func isRelease(version, commit string) bool {
	return releaseVersion().MatchString(version) && !strings.HasSuffix(version, "-dirty") &&
		hexCommit().MatchString(commit)
}

// blockedBy is why telemetry can't run for opts, whatever the config says.
func blockedBy(opts Options) string {
	switch {
	case opts.DoNotTrack:
		return "DO_NOT_TRACK is set"
	case !isRelease(opts.Version, opts.Commit):
		return "not a release build"
	case opts.Endpoint == "":
		return "this build has no collector endpoint"
	}

	return ""
}

// SetEnabled applies telemetry.enabled, as each config apply does. Turning
// it on starts a new window with nothing counted; turning it off drops
// what was counted.
func (r *Reporter) SetEnabled(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if on == r.on {
		return
	}

	r.on = on

	switch {
	case !on:
		r.restart()
		r.log.Info().Msg("usage telemetry disabled")
	case r.blocked != "":
		r.log.Info().Str("reason", r.blocked).Msg("usage telemetry enabled in the config but stays off")
	default:
		r.restart()
		r.log.Info().Msg("usage telemetry enabled; the daemon sends a daily usage report")
	}
}

// restart drops what was counted and starts a new window now. The caller
// holds r.mu.
func (r *Reporter) restart() {
	r.generation++
	r.since = r.now()
	r.usage = usage{clients: map[string]int64{}}
}

// active reports whether telemetry runs: on in the config and not blocked.
// The caller holds r.mu.
func (r *Reporter) active() bool { return r.on && r.blocked == "" }

// Search counts one search_tools call.
func (r *Reporter) Search() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.active() {
		r.usage.searches++
	}
}

// Call counts one call_tool call by the client that asserted the
// clientInfo name client.
func (r *Reporter) Call(client string, result store.CallResult) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.active() {
		return
	}

	r.usage.clients[clientName(client)]++

	switch result {
	case store.CallOK:
		r.usage.calls.OK++
	case store.CallToolError:
		r.usage.calls.ToolError++
	case store.CallRejected:
		r.usage.calls.Rejected++
	case store.CallFailed:
		r.usage.calls.Failed++
	case store.CallInputRequired:
		r.usage.calls.InputRequired++
	}
}

// Send sends a report of what was counted now, when telemetry runs, and
// does nothing otherwise. The daemon calls it on its schedule. Once the
// collector accepts it, the window starts again from now, with the counts
// it carried taken off; until then they wait for the next report.
func (r *Reporter) Send(ctx context.Context) error {
	r.mu.Lock()
	active := r.active()
	r.mu.Unlock()

	if !active {
		return nil
	}

	var cfg *config.Config

	if r.config != nil {
		cfg = r.config()
	}

	if cfg == nil {
		return nil
	}

	id, err := installID(r.idFile)

	if err != nil {
		return err
	}

	now := r.now()
	rep, gen := r.report(id, now)
	describe(&rep, cfg)
	rep.PoW.Date = now.UTC().Format(time.DateOnly)
	rep.PoW.Nonce, err = Solve(ctx, id, rep.PoW.Date, r.difficulty)

	if err != nil {
		return err
	}

	// Solving takes a second or more: telemetry may have been turned off,
	// or turned off and on, meanwhile. Then nothing goes out.
	if !r.current(gen) {
		return nil
	}

	if err := r.post(ctx, rep); err != nil {
		return err
	}

	r.sent(rep, gen, now)
	r.log.Debug().Int("window_hours", rep.WindowHours).Msg("usage report sent")

	return nil
}

// current reports whether telemetry still runs in the window gen began.
func (r *Reporter) current(gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.active() && r.generation == gen
}

// report is a report of the counts so far, without the config's fields or
// the proof-of-work, and the window's generation.
func (r *Reporter) report(id string, now time.Time) (Report, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	clients := make(map[string]int64, len(r.usage.clients))

	for k, v := range r.usage.clients {
		clients[k] = capped(v)
	}

	c := r.usage.calls

	return Report{
		Schema:      Schema,
		InstallID:   id,
		WindowHours: windowHours(r.since, now),
		Version:     r.version,
		Commit:      r.commit,
		Go:          goVersion(),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Searches:    capped(r.usage.searches),
		Calls: Calls{
			OK:            capped(c.OK),
			ToolError:     capped(c.ToolError),
			Rejected:      capped(c.Rejected),
			Failed:        capped(c.Failed),
			InputRequired: capped(c.InputRequired),
		},
		Clients: clients,
	}, r.generation
}

// sent takes rep's counts off and starts the window again at now, unless
// counting restarted while rep was in flight.
func (r *Reporter) sent(rep Report, gen uint64, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if gen != r.generation {
		return
	}

	u := &r.usage
	u.searches -= rep.Searches
	u.calls.OK -= rep.Calls.OK
	u.calls.ToolError -= rep.Calls.ToolError
	u.calls.Rejected -= rep.Calls.Rejected
	u.calls.Failed -= rep.Calls.Failed
	u.calls.InputRequired -= rep.Calls.InputRequired

	for k, v := range rep.Clients {
		u.clients[k] -= v

		if u.clients[k] <= 0 {
			delete(u.clients, k)
		}
	}

	r.since = now
}

// post sends rep to the collector; any 2xx is success.
func (r *Reporter) post(ctx context.Context, rep Report) error {
	body, err := json.Marshal(rep)

	if err != nil {
		return fmt.Errorf("encode usage report: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint+"/v1/report", bytes.NewReader(body))

	if err != nil {
		return fmt.Errorf("usage report request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wayfindr/"+r.version)
	resp, err := r.client.Do(req)

	if err != nil {
		// The cause, not the *url.Error around it: that names the endpoint
		// URL, which the debug log must not carry with any query (design §9).
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}

		return fmt.Errorf("send usage report: %w", err)
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("send usage report: collector answered %d", resp.StatusCode)
	}

	return nil
}

// goVersion is the Go release the binary was built with, without any
// experiment suffix ("go1.26.1 X:…" is go1.26.1).
func goVersion() string {
	v, _, _ := strings.Cut(runtime.Version(), " ")

	return v
}
