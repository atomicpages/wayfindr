package auth

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
)

const (
	// defaultDeviceInterval is how long a device code flow waits between
	// polls when the server names no interval (RFC 8628 §3.2).
	defaultDeviceInterval = 5 * time.Second
	// slowDownStep is what a slow_down answer adds to the interval, for
	// that poll and every later one (RFC 8628 §3.5).
	slowDownStep = 5 * time.Second
	// maxBackoffInterval caps the interval a poll that could not reach the
	// token endpoint doubles (see [transientPoll]), so the flow notices a
	// server that is back well before its deadline.
	maxBackoffInterval = time.Minute
)

// deviceCode is the device authorization grant (RFC 8628): the user enters
// a user code at the authorization server's verification URI, on any
// device, while a poller asks the token endpoint for the token. It needs no
// browser on the daemon's machine and no callback. A deviceCode serves one
// flow: begin keeps the device code for its poller.
type deviceCode struct {
	// stale is the config rule ([Flows.stale]), checked as the token is
	// stored.
	stale func(*flow) error

	// deviceCode and interval are the device authorization response's,
	// set by begin.
	deviceCode string
	interval   time.Duration
}

func (*deviceCode) grantType() string { return grantDeviceCode }

// check is the device code's one rule: the authorization server needs a
// device authorization endpoint, from its metadata or the config.
func (*deviceCode) check(b *Broker, connector string, _ config.Auth, disc Discovery) error {
	if disc.Endpoints.DeviceAuthorization != "" {
		return nil
	}

	return b.fail(CaseDeviceUnsupported, connector,
		"the authorization server metadata has no device_authorization_endpoint, and the config sets none", "", nil)
}

// begin sends the device authorization request (RFC 8628 §3.1) and returns
// the user code and verification URI the user is shown. The flow's expiry
// is brought forward to the device code's, if that is sooner.
func (k *deviceCode) begin(ctx context.Context, b *Broker, f *flow, oc oauthClient) (Prompt, error) {
	resp, err := k.authorize(ctx, f, oc)

	if err != nil {
		return Prompt{}, b.forgetRejectedDCR(ctx, f.Connector, f.client, f.seen, err)
	}

	k.deviceCode = resp.DeviceCode
	k.interval = defaultDeviceInterval

	if resp.Interval > 0 {
		k.interval = time.Duration(resp.Interval) * time.Second
	}

	if resp.ExpiresIn > 0 {
		b.flows.expireBy(f, b.now().Add(time.Duration(resp.ExpiresIn)*time.Second))
	}

	return Prompt{
		UserCode:                resp.UserCode,
		VerificationURI:         cmp.Or(resp.VerificationURI, resp.VerificationURL),
		VerificationURIComplete: resp.VerificationURIComplete,
	}, nil
}

// deviceAuthorization is a device authorization response (RFC 8628 §3.2).
type deviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	// VerificationURL is Google's name for verification_uri.
	VerificationURL string `json:"verification_url"`
	ExpiresIn       int64  `json:"expires_in"`
	Interval        int64  `json:"interval"`

	// Error is set on an error response (RFC 8628 §3.2, RFC 6749 §5.2).
	Error string `json:"error"`
}

// authorize sends the device authorization request through oc, so the
// client authenticates as at the token endpoint, and reads the response.
// The scope rule is the authorization code flow's, and so are the
// connector's authorize_params, which never set a protocol parameter. The
// verification URIs must be https or loopback http: one a hostile metadata
// document or server chose could send the user to a phishing page that
// asks for the code, so it is never shown or opened. Errors are [*Error]s;
// neither the body nor the device code is put in one.
func (k *deviceCode) authorize(ctx context.Context, f *flow, oc oauthClient) (deviceAuthorization, error) {
	endpoint := oc.endpoints.DeviceAuthorization
	form := url.Values{}

	for key, v := range f.auth.AuthorizeParams {
		if _, reserved := config.ReservedAuthorizeParam(key); !reserved {
			form.Set(key, v)
		}
	}

	if f.scope != "" {
		form.Set("scope", f.scope)
	}

	if f.auth.ResourceIndicator {
		form.Set("resource", f.resource)
	}

	resp, err := oc.post(ctx, endpoint, form)

	if err != nil {
		return deviceAuthorization{}, err
	}

	defer func() { _ = resp.Body.Close() }()
	what := "device authorization at " + endpoint
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse+1))

	if err != nil {
		return deviceAuthorization{}, oc.fail(CaseTransport, what+": read response", "", err)
	}

	var da deviceAuthorization

	if len(body) > maxTokenResponse || json.Unmarshal(body, &da) != nil {
		if resp.StatusCode/100 != 2 {
			return deviceAuthorization{}, oc.fail(CaseUnexpectedStatus, fmt.Sprintf("%s answered status %d", what, resp.StatusCode), "", nil)
		}

		return deviceAuthorization{}, oc.fail(CaseMalformedResponse, what+": the response is not JSON", "", nil)
	}

	if da.Error != "" {
		out := errorOutcome(da.Error)

		return deviceAuthorization{}, oc.fail(out.Case, fmt.Sprintf("%s answered status %d", what, resp.StatusCode), out.ProviderCode, nil)
	}

	if resp.StatusCode != http.StatusOK {
		return deviceAuthorization{}, oc.fail(CaseUnexpectedStatus, fmt.Sprintf("%s answered status %d", what, resp.StatusCode), "", nil)
	}

	if da.DeviceCode == "" || da.UserCode == "" || cmp.Or(da.VerificationURI, da.VerificationURL) == "" {
		return deviceAuthorization{}, oc.fail(CaseMalformedResponse,
			what+": the response lacks device_code, user_code, or verification_uri", "", nil)
	}

	for name, u := range map[string]string{
		"verification_uri":          cmp.Or(da.VerificationURI, da.VerificationURL),
		"verification_uri_complete": da.VerificationURIComplete,
	} {
		if u == "" {
			continue
		}

		if err := httpsOrLoopback(u); err != nil {
			return deviceAuthorization{}, oc.fail(CaseMalformedResponse, fmt.Sprintf(
				"%s: its %s %s is neither https nor loopback http, so it is not shown: it could be a phishing page",
				what, name, redact.URL(u)), "", nil)
		}
	}

	return da, nil
}

// armed starts the flow's one poller, which ends the flow.
func (k *deviceCode) armed(ctx context.Context, b *Broker, f *flow, oc oauthClient) error {
	return b.startPoller(ctx, f, func(ctx context.Context) { k.poll(ctx, b, f, oc) })
}

// poll asks the token endpoint for the token every interval (RFC 8628
// §3.4), through oc, so client authentication, the sender, and the
// [Inspectors] all apply, until the user has approved or denied, the
// device code expires, or the flow ends otherwise. authorization_pending
// asks again; slow_down adds [slowDownStep] to the interval from then on;
// access_denied ends the flow as [CaseAuthorizationDenied], expired_token
// as [CaseFlowExpired], and any other OAuth error with its Case. A poll
// that never got an answer about the grant (see [transientPoll]) doubles
// the interval and keeps polling until the flow's deadline. A token is
// stored through completeFlow, which checks under the row lock that the
// flow was neither revoked nor outdated meanwhile, so a token that comes
// after either is never stored. ctx ends when the flow does.
func (k *deviceCode) poll(ctx context.Context, b *Broker, f *flow, oc oauthClient) {
	interval := k.interval
	form := url.Values{"grant_type": {grantDeviceCode}, "device_code": {k.deviceCode}}

	if f.auth.ResourceIndicator {
		form.Set("resource", f.resource)
	}

	for {
		if b.sleep(ctx, interval) != nil {
			return // the flow ended, and whatever ended it set its result
		}

		if !b.now().Before(f.ExpiresAt) {
			k.fail(b, f, FlowExpired, b.fail(CaseFlowExpired, f.Connector, "the device code expired before it was entered", "", nil))

			return
		}

		tok, scope, err := oc.exchange(ctx, form)

		if ctx.Err() != nil {
			return
		}

		if err != nil {
			switch e, _ := AsError(err); {
			case e != nil && e.Case == CaseAuthorizationPending:
				continue
			case e != nil && e.Case == CaseSlowDown:
				interval += slowDownStep

				continue
			case e != nil && transientPoll(e):
				// RFC 8628 §3.5: "On encountering a connection timeout,
				// clients MUST unilaterally reduce their polling frequency
				// before retrying." Doubling is the exponential backoff the
				// RFC suggests; the slower interval stays, as slow_down's
				// does.
				interval = min(2*interval, max(interval, maxBackoffInterval))
				b.log.Debug().Str("connector", f.Connector).Str("case", string(e.Case)).
					Dur("interval", interval).Msg("device code poll failed, polling less often")

				continue
			case e != nil && e.Case == CaseFlowExpired:
				k.fail(b, f, FlowExpired, err)
			default:
				k.fail(b, f, FlowFailed, b.forgetRejectedDCR(ctx, f.Connector, f.client, f.seen, err))
			}

			return
		}

		if err := b.completeFlow(ctx, f, tok, scope, k.staleRule()); err != nil {
			k.fail(b, f, FlowFailed, err)

			return
		}

		if _, ok := b.flows.finish(f, FlowDone, nil); ok {
			b.log.Info().Str("connector", f.Connector).Str("issuer", redact.URL(f.disc.Issuer)).
				Str("grant_type", grantDeviceCode).Str("outcome", "ok").Msg("oauth flow completed")
		}

		return
	}
}

// transientPoll reports whether e, the error of a device code poll, is a
// failure to get an answer about the grant rather than one: the token
// endpoint could not be reached, answered a known transient code
// ([KindUnreachable]: transport, provider_unavailable; slow_down is
// handled on its own), or answered a 5xx with no OAuth error code. The
// flow goes on polling, less often; any other OAuth error ends it.
func transientPoll(e *Error) bool {
	switch {
	case e.Case == CaseSlowDown:
		return false
	case e.Kind == KindUnreachable:
		return true
	}

	return e.Case == CaseUnexpectedStatus && e.status >= 500
}

// staleRule is the config rule, or none when the kind was selected only to
// check a connector.
func (k *deviceCode) staleRule() func(*flow) error {
	if k.stale == nil {
		return func(*flow) error { return nil }
	}

	return k.stale
}

// fail ends f with state and err, and logs it if that ended it: a flow a
// revoke or a config change ended first keeps its result.
func (k *deviceCode) fail(b *Broker, f *flow, state FlowState, err error) {
	if _, ok := b.flows.finish(f, state, err); !ok {
		return
	}

	ev := b.log.Warn().Str("connector", f.Connector).Str("issuer", redact.URL(f.disc.Issuer)).
		Str("grant_type", grantDeviceCode).Str("outcome", outcome(err))

	if e, ok := AsError(err); ok {
		ev = ev.Str("case", string(e.Case)).Str("provider_code", e.ProviderCode)
	}

	ev.Msg("oauth flow failed")
}
