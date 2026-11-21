package fakeas

import (
	"crypto/rand"
	"net/http"
	"slices"
	"strings"
)

// GrantDeviceCode is the device code grant type (RFC 8628 §3.4).
const GrantDeviceCode = "urn:ietf:params:oauth:grant-type:device_code"

// deviceConfig is the device authorization endpoint's configuration.
type deviceConfig struct {
	advertised      bool
	verificationURI string
	// timed is set by WithDeviceTiming, which sets expiresIn and interval.
	timed     bool
	expiresIn int
	interval  int
}

// WithDeviceCode advertises the device authorization endpoint (RFC 8628)
// in the metadata, as device_authorization_endpoint and in
// grant_types_supported. The endpoint is served either way, so a client
// can reach it through config alone.
//
// The endpoint has no verification page: a test plays the user with
// [Server.ApproveDevice] or [Server.DenyDevice], and the token endpoint
// answers the device_code grant with authorization_pending until then.
func WithDeviceCode() Option {
	return func(c *config) { c.device.advertised = true }
}

// WithDeviceVerificationURI makes the device authorization response name u
// as its verification_uri and, with ?user_code= added,
// verification_uri_complete, instead of a page on the fake.
func WithDeviceVerificationURI(u string) Option {
	return func(c *config) { c.device.verificationURI = u }
}

// WithDeviceTiming sets the device authorization response's expires_in and
// interval, in seconds; zero leaves a field out. The defaults are 600 and
// 5. The fake does not enforce either: [Server.ExpireDevice] expires a code.
func WithDeviceTiming(expiresIn, interval int) Option {
	return func(c *config) {
		c.device.timed, c.device.expiresIn, c.device.interval = true, expiresIn, interval
	}
}

// DeviceAuthorization records a device authorization request.
type DeviceAuthorization struct {
	ClientID string
	// AuthMethod is how the client authenticated, as for a token request.
	AuthMethod string
	Scope      string
	Resource   string
	// UserCode is the user code it was answered with, or empty if it
	// failed.
	UserCode string
	// Extra holds the parameters the fake doesn't read itself, or is nil
	// when there are none.
	Extra map[string]string
	// DPoP is whether it carried a DPoP header, which RFC 9449 does not
	// ask of the device authorization request.
	DPoP bool
	// Error is the OAuth error it failed with, or empty.
	Error string
}

// deviceState is where a device code stands.
type deviceState int

const (
	devicePending deviceState = iota
	deviceApproved
	deviceDenied
	deviceExpired
)

// device is an issued device code.
type device struct {
	grant
	userCode string
	state    deviceState
	// slowDown makes the next poll answer slow_down.
	slowDown bool
}

// ApproveDevice plays the user entering userCode and allowing access: the
// next poll gets the token. It reports false for an unknown user code.
func (s *Server) ApproveDevice(userCode string) bool {
	return s.setDevice(userCode, func(d *device) { d.state = deviceApproved })
}

// DenyDevice plays the user declining: the next poll gets access_denied.
func (s *Server) DenyDevice(userCode string) bool {
	return s.setDevice(userCode, func(d *device) { d.state = deviceDenied })
}

// ExpireDevice expires userCode's device code: the next poll gets
// expired_token.
func (s *Server) ExpireDevice(userCode string) bool {
	return s.setDevice(userCode, func(d *device) { d.state = deviceExpired })
}

// SlowDownDevice makes the next poll of userCode's device code answer
// slow_down, whatever its state.
func (s *Server) SlowDownDevice(userCode string) bool {
	return s.setDevice(userCode, func(d *device) { d.slowDown = true })
}

// DeviceAuthorizations returns the device authorization requests seen so
// far, in order.
func (s *Server) DeviceAuthorizations() []DeviceAuthorization {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.deviceReqs)
}

func (s *Server) setDevice(userCode string, set func(*device)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, d := range s.devices {
		if d.userCode == userCode {
			set(d)

			return true
		}
	}

	return false
}

// newUserCode is an 8-letter user code, XXXX-XXXX, as RFC 8628 §6.1
// suggests.
func newUserCode() string {
	t := strings.ToUpper(rand.Text())

	return t[:4] + "-" + t[4:8]
}

// serveDeviceAuthorization is RFC 8628 §3.1: it authenticates the client
// as the token endpoint does and answers with a new device code.
func (s *Server) serveDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, oauthError{"invalid_request", "bad form body"})

		return
	}

	f := r.PostForm
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := DeviceAuthorization{Scope: f.Get("scope"), Resource: f.Get("resource"), DPoP: r.Header.Get("DPoP") != ""}

	for k := range f {
		switch k {
		case "client_id", "client_secret", "client_assertion", "client_assertion_type", "scope", "resource":
		default:
			if rec.Extra == nil {
				rec.Extra = map[string]string{}
			}

			rec.Extra[k] = f.Get(k)
		}
	}

	var tr TokenRequest
	c, ok := s.clientAuth(r, &tr)
	rec.ClientID, rec.AuthMethod = c.ID, tr.AuthMethod
	fail := func(status int, code, desc string) {
		rec.Error = code
		s.deviceReqs = append(s.deviceReqs, rec)
		writeJSON(w, status, oauthError{code, desc})
	}

	switch {
	case !ok:
		fail(http.StatusUnauthorized, "invalid_client", "client authentication failed")

		return
	case rec.Resource != "" && rec.Resource != s.ResourceURL:
		fail(http.StatusBadRequest, "invalid_target", "unknown resource")

		return
	}

	d := &device{
		grant:    grant{clientID: c.ID, scope: rec.Scope, resource: rec.Resource},
		userCode: newUserCode(),
	}
	code := newSecret("dc")
	s.devices[code] = d
	rec.UserCode = d.userCode
	s.deviceReqs = append(s.deviceReqs, rec)

	verify := s.cfg.device.verificationURI

	if verify == "" {
		verify = s.Issuer + "/device"
	}

	resp := map[string]any{
		"device_code":               code,
		"user_code":                 d.userCode,
		"verification_uri":          verify,
		"verification_uri_complete": verify + "?user_code=" + d.userCode,
	}

	expiresIn, interval := 600, 5

	if s.cfg.device.timed {
		expiresIn, interval = s.cfg.device.expiresIn, s.cfg.device.interval
	}

	if expiresIn > 0 {
		resp["expires_in"] = expiresIn
	}

	if interval > 0 {
		resp["interval"] = interval
	}

	writeJSON(w, http.StatusOK, resp)
}

// redeemDevice answers a device_code grant by client c for code (RFC 8628
// §3.5): the grant once the user approved, which spends the code, or the
// error code and description. s.mu must be held.
func (s *Server) redeemDevice(c Client, code string) (*grant, string, string) {
	d := s.devices[code]

	switch {
	case d == nil || d.clientID != c.ID:
		return nil, "invalid_grant", "unknown or used device code"
	case len(c.GrantTypes) > 0 && !slices.Contains(c.GrantTypes, GrantDeviceCode):
		return nil, "unauthorized_client", "the client is not registered for the device code grant"
	case d.slowDown:
		d.slowDown = false

		return nil, "slow_down", ""
	}

	switch d.state {
	case devicePending:
		return nil, "authorization_pending", ""
	case deviceDenied:
		delete(s.devices, code)

		return nil, "access_denied", ""
	case deviceExpired:
		delete(s.devices, code)

		return nil, "expired_token", ""
	case deviceApproved:
	}

	delete(s.devices, code)
	g := d.grant

	return &g, "", ""
}
