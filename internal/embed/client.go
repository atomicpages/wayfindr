package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/redact"
)

// ErrBadResponse means a 2xx response did not have the shape the
// embeddings API promises (undecodable, wrong item count, bad indexes).
var ErrBadResponse = errors.New("embed: malformed provider response")

// DimensionError means the provider returned vectors of a different length
// than embedding.dimensions: usually a provider that ignores the
// "dimensions" field, or a config that names the wrong size. It is a config
// problem, so retrying will not help.
type DimensionError struct {
	Want, Got int
}

func (e *DimensionError) Error() string {
	return fmt.Sprintf("embed: provider returned %d-dimension vectors, embedding.dimensions is %d", e.Got, e.Want)
}

// maxErrorBody is the most bytes of a non-2xx body kept in a StatusError.
const maxErrorBody = 512

// StatusError is a non-2xx response from the provider. A 401 or 403
// usually means a wrong api_key; a 400 may mean the provider rejects the
// "dimensions" field or the model name.
type StatusError struct {
	StatusCode int
	// Body is the start of the response body (at most 512 bytes), with the
	// api_key and any URL query strings replaced by redact.Mask, so it is
	// safe to log or store.
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("embed: provider returned HTTP %d: %s", e.StatusCode, e.Body)
}

// Config is what a Client needs from the [embedding] table (design §4),
// with api_key already resolved.
type Config struct {
	// BaseURL is the provider's OpenAI-compatible base, e.g.
	// "https://api.openai.com/v1"; requests go to BaseURL + "/embeddings".
	BaseURL string
	Model   string
	// Dimensions is the vector length every response must have. It is sent
	// as the request's "dimensions" field.
	Dimensions int
	// APIKey is the resolved key (not a ${…} template). Empty sends no
	// Authorization header.
	APIKey string
}

// Client calls one embedding provider. It is safe for concurrent use.
type Client struct {
	cfg      Config
	endpoint string
	modelID  string
	http     *http.Client
	log      zerolog.Logger
}

// New returns a Client for cfg. The http.Client is used as given, so its
// own Timeout (if any) still applies; callers normally leave it zero and
// bound each call with a context deadline instead.
func New(cfg Config, client *http.Client, log zerolog.Logger) (*Client, error) {
	if cfg.Model == "" {
		return nil, errors.New("embed: model is required")
	}

	if cfg.Dimensions <= 0 {
		return nil, errors.New("embed: dimensions must be positive")
	}

	u, err := url.Parse(cfg.BaseURL)

	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("embed: base_url must be an absolute http(s) URL")
	}

	if client == nil {
		return nil, errors.New("embed: http client is required")
	}

	return &Client{
		cfg:      cfg,
		endpoint: u.JoinPath("embeddings").String(),
		modelID:  ModelID(cfg.BaseURL, cfg.Model, cfg.Dimensions),
		http:     client,
		log:      log,
	}, nil
}

// ModelID is ModelID(cfg.BaseURL, cfg.Model, cfg.Dimensions).
func (c *Client) ModelID() string { return c.modelID }

// Model is the configured model name.
func (c *Client) Model() string { return c.cfg.Model }

type wireRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions"`
}

type wireResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// BatchSize is the most inputs sent in one request.
const BatchSize = 64

// Embed returns one vector per input, in input order. It sends the inputs in
// requests of at most BatchSize, one after another, and fails on the first
// failed batch (no partial result). Empty inputs return nil without a
// request. The deadline comes from ctx.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}

	out := make([][]float32, 0, len(inputs))

	for start := 0; start < len(inputs); start += BatchSize {
		end := min(start+BatchSize, len(inputs))
		vecs, err := c.embedBatch(ctx, inputs[start:end])

		if err != nil {
			return nil, err
		}

		out = append(out, vecs...)
	}

	return out, nil
}

// errorBody reads a bounded prefix of a non-2xx body and scrubs it. It
// reads more than it keeps so a key that straddles the cut is still found
// whole before truncation.
func (c *Client) errorBody(r io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(r, int64(4*maxErrorBody+len(c.cfg.APIKey))))
	s := string(raw)

	if c.cfg.APIKey != "" {
		s = strings.ReplaceAll(s, c.cfg.APIKey, redact.Mask)
	}

	s = strings.TrimSpace(redact.URLQueries(s))

	if len(s) > maxErrorBody {
		s = strings.ToValidUTF8(s[:maxErrorBody], "")
	}

	return s
}

func (c *Client) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	body, err := json.Marshal(wireRequest{Model: c.cfg.Model, Input: batch, Dimensions: c.cfg.Dimensions})

	if err != nil {
		return nil, fmt.Errorf("embed: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))

	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	start := time.Now()
	resp, err := c.http.Do(req)

	if err != nil {
		// *url.Error quotes the full URL; replace it with the redacted one
		// and keep the cause, so errors.Is(err, context.DeadlineExceeded)
		// still works.
		var ue *url.Error

		if errors.As(err, &ue) {
			err = ue.Err
		}

		return nil, fmt.Errorf("embed: post %s: %w", redact.URLQueries(c.endpoint), err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{StatusCode: resp.StatusCode, Body: c.errorBody(resp.Body)}
	}

	var wr wireResponse

	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		return nil, fmt.Errorf("%w: decode: %w", ErrBadResponse, err)
	}

	if len(wr.Data) != len(batch) {
		return nil, fmt.Errorf("%w: %d embeddings for %d inputs", ErrBadResponse, len(wr.Data), len(batch))
	}

	out := make([][]float32, len(batch))

	for _, d := range wr.Data {
		if d.Index < 0 || d.Index >= len(batch) || out[d.Index] != nil {
			return nil, fmt.Errorf("%w: bad or repeated index %d", ErrBadResponse, d.Index)
		}

		if len(d.Embedding) != c.cfg.Dimensions {
			return nil, &DimensionError{Want: c.cfg.Dimensions, Got: len(d.Embedding)}
		}

		out[d.Index] = d.Embedding
	}

	c.log.Debug().Str("model", c.cfg.Model).Int("count", len(batch)).
		Int64("duration_ms", time.Since(start).Milliseconds()).Msg("embedding batch done")

	return out, nil
}
