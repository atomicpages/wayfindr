package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/atomicpages/wayfindr/internal/upstream"
)

// maxErrorBody bounds how much of an error response Client reads to show.
const maxErrorBody = 4 << 10

// Client calls the admin API: one method per endpoint, so its callers never
// build a path or a body.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient returns a Client for the daemon at baseURL (such as
// "http://127.0.0.1:7777") that presents token as a bearer; an empty token
// sends none. hc nil means http.DefaultClient.
func NewClient(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}

	return &Client{base: strings.TrimSuffix(baseURL, "/"), token: token, http: hc}
}

// Status calls GET /admin/status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.do(ctx, http.MethodGet, pathStatus, nil, &s)

	return s, err
}

// PutSecret calls PUT /admin/secrets/{name}: it stores value as secret name
// and reports the connectors that were waiting on it. It returns once each
// of their reconnects has settled.
func (c *Client) PutSecret(ctx context.Context, name, value string) (SecretSet, error) {
	var res SecretSet
	err := c.do(ctx, http.MethodPut, pathSecrets+"/"+url.PathEscape(name), SecretValue{Value: value}, &res)

	return res, err
}

// DeleteSecret calls DELETE /admin/secrets/{name}: it removes the stored
// secret. A name with no secret is an *APIError with status 404.
func (c *Client) DeleteSecret(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, pathSecrets+"/"+url.PathEscape(name), nil, nil)
}

// ListSecrets calls GET /admin/secrets: the stored secrets' names, never
// their values, and what in the running config refers to each.
func (c *Client) ListSecrets(ctx context.Context) (SecretList, error) {
	var res SecretList
	err := c.do(ctx, http.MethodGet, pathSecrets, nil, &res)

	return res, err
}

// StartAuth calls POST /admin/auth/{connector}: it starts the connector's
// auth flow, as start asks, and returns what the user is shown: the URL to
// open, or the user code and where to enter it.
func (c *Client) StartAuth(ctx context.Context, connector string, start AuthStart) (AuthFlow, error) {
	var in any

	if start != (AuthStart{}) {
		in = start
	}

	var f AuthFlow
	err := c.do(ctx, http.MethodPost, pathAuth+url.PathEscape(connector), in, &f)

	return f, err
}

// WaitAuth calls GET /admin/auth/flows/{id}?wait=: it reports the flow once
// it has ended, or as pending after wait (at most MaxFlowWait).
func (c *Client) WaitAuth(ctx context.Context, id string, wait time.Duration) (AuthFlowStatus, error) {
	var s AuthFlowStatus
	err := c.do(ctx, http.MethodGet, pathFlows+url.PathEscape(id)+"?wait="+wait.String(), nil, &s)

	return s, err
}

// Revoke calls DELETE /admin/credentials/{connector}: it deletes the
// connector's stored credential and returns once the connector has
// settled in needs_auth. A connector with no credential is an error with
// status 404.
func (c *Client) Revoke(ctx context.Context, connector string) (Revoked, error) {
	var r Revoked
	err := c.do(ctx, http.MethodDelete, pathCreds+url.PathEscape(connector), nil, &r)

	return r, err
}

// Settled is how a wait for a connector ended ([Client.WaitConnector]).
type Settled struct {
	// Connector is the connector's row once it is listed and past
	// starting.
	Connector Connector
	// ConfigError is a config error the daemon did not have before the
	// wait: it could not load the new config.
	ConfigError string
	// TimedOut is true when the limit passed first.
	TimedOut bool
}

// WaitConnector polls Status every interval until connector is listed and
// no longer starting, or the daemon reports a config error other than
// prevConfigErr: one left from an earlier edit must not end the wait. When
// limit passes first it returns TimedOut; a cancelled ctx is an error.
func (c *Client) WaitConnector(ctx context.Context, connector, prevConfigErr string,
	interval, limit time.Duration,
) (Settled, error) {
	wctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		s, err := c.Status(wctx)

		switch {
		case err != nil && wctx.Err() != nil && ctx.Err() == nil:
			return Settled{TimedOut: true}, nil
		case err != nil:
			return Settled{}, err
		case s.ConfigError != "" && s.ConfigError != prevConfigErr:
			return Settled{ConfigError: s.ConfigError}, nil
		}

		for _, cn := range s.Connectors {
			if cn.Name == connector && cn.State != upstream.StateStarting {
				return Settled{Connector: cn}, nil
			}
		}

		select {
		case <-wctx.Done():
			if err := ctx.Err(); err != nil {
				return Settled{}, err
			}

			return Settled{TimedOut: true}, nil
		case <-tick.C:
		}
	}
}

// APIError is a non-2xx answer from the admin API. Its text is the daemon's
// message alone; Method, Path, and Status are for logs and for callers that
// branch on the status.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

// Error returns the daemon's message, or a stand-in when it sent none.
func (e *APIError) Error() string {
	if msg := strings.TrimSpace(e.Message); msg != "" {
		return msg
	}

	return "the daemon answered " + strings.ToLower(http.StatusText(e.Status))
}

// do sends method path with in, if not nil, as its JSON body, and decodes a
// 2xx response's JSON body into out, unless out is nil or the response is
// 204 No Content. A transport error is wrapped, so a
// caller can tell a failed dial (*net.OpError) from the rest. A non-2xx
// response is an *APIError. ctx
// bounds the whole call, long polls included.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader

	if in != nil {
		b, err := json.Marshal(in)

		if err != nil {
			return fmt.Errorf("admin API %s %s: %w", method, path, err)
		}

		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)

	if err != nil {
		return fmt.Errorf("admin API %s %s: %w", method, path, err)
	}

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)

	if err != nil {
		return fmt.Errorf("admin API %s %s: %w", method, path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))

		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: strings.TrimSpace(string(msg))}
	}

	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("admin API %s %s: decode response: %w", method, path, err)
	}

	return nil
}
