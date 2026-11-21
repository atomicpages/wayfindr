package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/embed"
)

// request is what the fake provider records about each call.
type request struct {
	Method, Path, Authorization, ContentType string
	Body                                     wireRequest
}

type wireRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions *int     `json:"dimensions"`
}

type wireItem struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// provider is a fake OpenAI-compatible embeddings endpoint. By default it
// answers each input with a vector of `dims` copies of float32(n), where n
// is the input's position across all calls (0, 1, 2, …) — so the result
// reveals whether order survived batching.
type provider struct {
	t    *testing.T
	dims int
	// respond, when set, replaces the default handler body.
	respond func(w http.ResponseWriter, r *http.Request, body wireRequest)

	mu   sync.Mutex
	reqs []request
	seen int
}

func (p *provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)

	if err != nil {
		p.t.Errorf("read body: %v", err)
	}

	var body wireRequest

	if err := json.Unmarshal(raw, &body); err != nil {
		p.t.Errorf("decode body %q: %v", raw, err)
	}

	p.mu.Lock()
	p.reqs = append(p.reqs, request{
		Method: r.Method, Path: r.URL.Path,
		Authorization: r.Header.Get("Authorization"),
		ContentType:   r.Header.Get("Content-Type"),
		Body:          body,
	})
	start := p.seen
	p.seen += len(body.Input)
	p.mu.Unlock()

	if p.respond != nil {
		p.respond(w, r, body)

		return
	}

	writeEmbeddings(w, start, len(body.Input), p.dims)
}

func (p *provider) requests() []request {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]request(nil), p.reqs...)
}

func writeEmbeddings(w http.ResponseWriter, start, n, dims int) {
	items := make([]wireItem, n)

	for i := range items {
		items[i] = wireItem{Object: "embedding", Index: i, Embedding: vec(start+i, dims)}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items, "model": "m"})
}

func vec(n, dims int) []float32 {
	v := make([]float32, dims)

	for i := range v {
		v[i] = float32(n)
	}

	return v
}

func inputs(n int) []string {
	in := make([]string, n)

	for i := range in {
		in[i] = "text " + string(rune('a'+i%26))
	}

	return in
}

func intp(i int) *int { return &i }

func newClient(t *testing.T, cfg embed.Config, srv *httptest.Server) *embed.Client {
	t.Helper()

	if cfg.BaseURL == "" {
		cfg.BaseURL = srv.URL + "/v1"
	}

	if cfg.Model == "" {
		cfg.Model = "test-model"
	}

	c, err := embed.New(cfg, srv.Client(), zerolog.Nop())

	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}

func TestEmbedSendsOpenAIRequest(t *testing.T) {
	p := &provider{t: t, dims: 3}
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := newClient(t, embed.Config{Model: "text-embedding-3-small", Dimensions: 3}, srv)

	got, err := c.Embed(context.Background(), []string{"hello", "world"})

	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	want := [][]float32{{0, 0, 0}, {1, 1, 1}}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("vectors (-want +got):\n%s", diff)
	}

	wantReqs := []request{{
		Method: http.MethodPost, Path: "/v1/embeddings", ContentType: "application/json",
		Body: wireRequest{Model: "text-embedding-3-small", Input: []string{"hello", "world"}, Dimensions: intp(3)},
	}}

	if diff := cmp.Diff(wantReqs, p.requests()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

func TestEmbedBatchesOf64PreservingOrder(t *testing.T) {
	tests := []struct {
		name      string
		n         int
		wantSizes []int
	}{
		{"empty input makes no request", 0, nil},
		{"one", 1, []int{1}},
		{"exactly one batch", 64, []int{64}},
		{"one over", 65, []int{64, 1}},
		{"several batches", 150, []int{64, 64, 22}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &provider{t: t, dims: 2}
			srv := httptest.NewServer(p)
			defer srv.Close()
			c := newClient(t, embed.Config{Dimensions: 2}, srv)
			in := inputs(tt.n)

			got, err := c.Embed(context.Background(), in)

			if err != nil {
				t.Fatalf("Embed: %v", err)
			}

			var want [][]float32

			for i := range tt.n {
				want = append(want, vec(i, 2))
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("vectors (-want +got):\n%s", diff)
			}

			var sizes []int
			var sent []string

			for _, r := range p.requests() {
				sizes = append(sizes, len(r.Body.Input))
				sent = append(sent, r.Body.Input...)
			}

			if diff := cmp.Diff(tt.wantSizes, sizes); diff != "" {
				t.Errorf("batch sizes (-want +got):\n%s", diff)
			}

			if tt.n > 0 {
				if diff := cmp.Diff(in, sent); diff != "" {
					t.Errorf("inputs sent (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestEmbedAuthorizationHeader(t *testing.T) {
	tests := []struct {
		name   string
		apiKey string
		want   string
	}{
		{"api key is sent as a bearer token", "sk-test-123", "Bearer sk-test-123"},
		{"no api key sends no header", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &provider{t: t, dims: 1}
			srv := httptest.NewServer(p)
			defer srv.Close()
			c := newClient(t, embed.Config{Dimensions: 1, APIKey: tt.apiKey}, srv)

			if _, err := c.Embed(context.Background(), inputs(65)); err != nil {
				t.Fatalf("Embed: %v", err)
			}

			for i, r := range p.requests() {
				if r.Authorization != tt.want {
					t.Errorf("request %d Authorization = %q, want %q", i, r.Authorization, tt.want)
				}
			}
		})
	}
}

func TestEmbedNon2xxReturnsStatusErrorWithoutKey(t *testing.T) {
	const key = "sk-live-SECRET-0123456789"
	tests := []struct {
		name     string
		status   int
		body     string
		wantBody string
	}{
		{
			"provider echoes the key",
			http.StatusUnauthorized,
			`{"error":{"message":"Incorrect API key provided: sk-live-SECRET-0123456789"}}`,
			`{"error":{"message":"Incorrect API key provided: REDACTED"}}`,
		},
		{
			"provider echoes the Authorization header",
			http.StatusForbidden,
			"denied for Bearer sk-live-SECRET-0123456789",
			"denied for Bearer REDACTED",
		},
		{
			"url query in the body is redacted",
			http.StatusBadRequest,
			"see https://docs.example/err?token=abc",
			"see https://docs.example/err?REDACTED",
		},
		{
			"server error body is kept",
			http.StatusInternalServerError,
			"model not loaded",
			"model not loaded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &provider{t: t}
			p.respond = func(w http.ResponseWriter, _ *http.Request, _ wireRequest) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}
			srv := httptest.NewServer(p)
			defer srv.Close()
			c := newClient(t, embed.Config{Dimensions: 1, APIKey: key}, srv)

			_, err := c.Embed(context.Background(), []string{"a"})

			var se *embed.StatusError

			if !errors.As(err, &se) {
				t.Fatalf("Embed error = %v, want *embed.StatusError", err)
			}

			if diff := cmp.Diff(&embed.StatusError{StatusCode: tt.status, Body: tt.wantBody}, se); diff != "" {
				t.Errorf("StatusError (-want +got):\n%s", diff)
			}

			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error text leaks the api key: %q", err.Error())
			}
		})
	}
}

func TestEmbedStatusErrorBodyIsBounded(t *testing.T) {
	p := &provider{t: t}
	p.respond = func(w http.ResponseWriter, _ *http.Request, _ wireRequest) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, strings.Repeat("x", 10_000))
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := newClient(t, embed.Config{Dimensions: 1}, srv)

	_, err := c.Embed(context.Background(), []string{"a"})

	var se *embed.StatusError

	if !errors.As(err, &se) {
		t.Fatalf("Embed error = %v, want *embed.StatusError", err)
	}

	if len(se.Body) != 512 {
		t.Errorf("len(Body) = %d, want 512", len(se.Body))
	}
}

func TestEmbedHonoursContextDeadline(t *testing.T) {
	p := &provider{t: t}
	p.respond = func(_ http.ResponseWriter, r *http.Request, _ wireRequest) {
		<-r.Context().Done() // never answer; wait for the client to give up
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := newClient(t, embed.Config{Dimensions: 1, APIKey: "sk-SECRET"}, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Embed(ctx, []string{"a"})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Embed error = %v, want context.DeadlineExceeded", err)
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Embed took %v after a 50ms deadline", elapsed)
	}

	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error text leaks the api key: %q", err.Error())
	}
}

func TestEmbedTransportErrorRedactsURLQuery(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL + "/v1?api-version=SECRET"
	srv.Close() // nothing listens any more: connection refused

	c, err := embed.New(embed.Config{BaseURL: base, Model: "m", Dimensions: 1}, http.DefaultClient, zerolog.Nop())

	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Embed(context.Background(), []string{"a"})

	if err == nil {
		t.Fatal("Embed succeeded against a closed server")
	}

	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error text leaks the url query: %q", err.Error())
	}
}

func TestNewRejectsUnusableConfig(t *testing.T) {
	ok := embed.Config{BaseURL: "http://localhost:11434/v1", Model: "m", Dimensions: 8}
	tests := []struct {
		name   string
		mutate func(*embed.Config)
		client *http.Client
	}{
		{"missing model", func(c *embed.Config) { c.Model = "" }, http.DefaultClient},
		{"zero dimensions", func(c *embed.Config) { c.Dimensions = 0 }, http.DefaultClient},
		{"negative dimensions", func(c *embed.Config) { c.Dimensions = -1 }, http.DefaultClient},
		{"relative base url", func(c *embed.Config) { c.BaseURL = "/v1" }, http.DefaultClient},
		{"non-http base url", func(c *embed.Config) { c.BaseURL = "ftp://h/v1" }, http.DefaultClient},
		{"nil http client", func(*embed.Config) {}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ok
			tt.mutate(&cfg)

			if _, err := embed.New(cfg, tt.client, zerolog.Nop()); err == nil {
				t.Errorf("New(%+v) succeeded, want an error", cfg)
			}
		})
	}
}

func TestClientModelIDMatchesConfig(t *testing.T) {
	c, err := embed.New(embed.Config{BaseURL: "https://api.openai.com/v1/", Model: "text-embedding-3-small", Dimensions: 1536, APIKey: "k"},
		http.DefaultClient, zerolog.Nop())

	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const want = "ca7ddf93a4304edd6af3c5186087b6ff02b1287ba95d6f4f08e8614a93da2ff7" // see TestModelID

	if got := c.ModelID(); got != want {
		t.Errorf("ModelID() = %s, want %s", got, want)
	}
}

func TestEmbedNeverLogsAPIKey(t *testing.T) {
	const key = "sk-live-SECRET-0123456789"
	p := &provider{t: t, dims: 1}
	p.respond = func(w http.ResponseWriter, _ *http.Request, body wireRequest) {
		if len(p.requests()) == 2 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "bad key "+key)

			return
		}

		writeEmbeddings(w, 0, len(body.Input), 1)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	var logs strings.Builder
	log := zerolog.New(&logs).Level(zerolog.TraceLevel)
	c, err := embed.New(embed.Config{BaseURL: srv.URL + "/v1", Model: "m", Dimensions: 1, APIKey: key}, srv.Client(), log)

	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, _ = c.Embed(context.Background(), inputs(100))

	if strings.Contains(logs.String(), "SECRET") {
		t.Errorf("logs leak the api key:\n%s", logs.String())
	}
}

func TestEmbedRejectsDimensionMismatch(t *testing.T) {
	// The provider ignores "dimensions" and returns its native size, as some
	// OpenAI-compatible servers do. The second batch is the bad one, to show
	// the whole call fails rather than returning a partial result.
	p := &provider{t: t}
	p.respond = func(w http.ResponseWriter, _ *http.Request, body wireRequest) {
		dims := 4

		if len(p.requests()) == 2 {
			dims = 8
		}

		writeEmbeddings(w, 0, len(body.Input), dims)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := newClient(t, embed.Config{Dimensions: 4}, srv)

	got, err := c.Embed(context.Background(), inputs(70))

	if got != nil {
		t.Errorf("Embed returned %d vectors with an error, want none", len(got))
	}

	var de *embed.DimensionError

	if !errors.As(err, &de) {
		t.Fatalf("Embed error = %v, want *embed.DimensionError", err)
	}

	if diff := cmp.Diff(&embed.DimensionError{Want: 4, Got: 8}, de); diff != "" {
		t.Errorf("DimensionError (-want +got):\n%s", diff)
	}
}

func TestEmbedRejectsMalformedResponse(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", `<html>oops</html>`},
		{"too few items", `{"data":[{"index":0,"embedding":[1]}]}`},
		{"too many items", `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[1]},{"index":2,"embedding":[1]}]}`},
		{"repeated index", `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[1]}]}`},
		{"index out of range", `{"data":[{"index":0,"embedding":[1]},{"index":2,"embedding":[1]}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &provider{t: t}
			p.respond = func(w http.ResponseWriter, _ *http.Request, _ wireRequest) {
				_, _ = io.WriteString(w, tt.body)
			}
			srv := httptest.NewServer(p)
			defer srv.Close()
			c := newClient(t, embed.Config{Dimensions: 1}, srv)

			_, err := c.Embed(context.Background(), []string{"a", "b"})

			if !errors.Is(err, embed.ErrBadResponse) {
				t.Errorf("Embed error = %v, want ErrBadResponse", err)
			}
		})
	}
}

func TestEmbedOrdersByResponseIndex(t *testing.T) {
	// The spec makes data[].index authoritative; a provider may return
	// items in any order.
	p := &provider{t: t, dims: 1}
	p.respond = func(w http.ResponseWriter, _ *http.Request, _ wireRequest) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []wireItem{
			{Index: 2, Embedding: []float32{20}},
			{Index: 0, Embedding: []float32{0}},
			{Index: 1, Embedding: []float32{10}},
		}})
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := newClient(t, embed.Config{Dimensions: 1}, srv)

	got, err := c.Embed(context.Background(), []string{"a", "b", "c"})

	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if diff := cmp.Diff([][]float32{{0}, {10}, {20}}, got); diff != "" {
		t.Errorf("vectors (-want +got):\n%s", diff)
	}
}
