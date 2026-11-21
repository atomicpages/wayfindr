// Package auth is the OAuth broker for upstream connectors (design §7.3).
// This file holds the discovery chain of ADR 0007: given a connector's auth
// config, its MCP URL, and an optional WWW-Authenticate challenge, it finds
// the Protected Resource Metadata, the authorization server metadata, and the
// endpoints to use, caching every fetched document in the store.
package auth

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
	"github.com/atomicpages/wayfindr/internal/store"
)

// Source says where a [Discovery]'s endpoints came from.
type Source string

// Endpoint sources.
const (
	// SourceConfig: authorization_endpoint and token_endpoint in config.
	SourceConfig Source = "config"
	// SourceMetadata: authorization server metadata, found through PRM or
	// the authorization_server override.
	SourceMetadata Source = "metadata"
	// SourceLegacyMetadata: no PRM; metadata at the MCP origin (2025-03-26).
	SourceLegacyMetadata Source = "legacy_metadata"
	// SourceLegacyDefaults: no PRM and no metadata; the default /authorize,
	// /token, and /register on the MCP origin (2025-03-26).
	SourceLegacyDefaults Source = "legacy_defaults"
)

// Endpoints are the OAuth endpoints a connector uses. Registration is empty
// when there is no DCR endpoint, and DeviceAuthorization when there is no
// device authorization endpoint (RFC 8628 §3.1): it is the config's
// device_authorization_endpoint, else the metadata's.
type Endpoints struct {
	Authorization       string
	Token               string
	Registration        string
	DeviceAuthorization string
}

// MetadataExtensions are the authorization server metadata fields that
// go-sdk's oauthex.AuthServerMeta does not have, read from the same
// document by [parseMetadataExtensions].
type MetadataExtensions struct {
	// DeviceAuthorizationEndpoint is RFC 8628 §4's
	// device_authorization_endpoint.
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint,omitempty"`
	// DPoPSigningAlgValuesSupported is RFC 9449 §5.1's
	// dpop_signing_alg_values_supported: nil when the metadata leaves it
	// out, and empty when it lists none.
	DPoPSigningAlgValuesSupported []string `json:"dpop_signing_alg_values_supported,omitempty"`
	// MTLSEndpointAliases are RFC 8705 §5's mtls_endpoint_aliases: the
	// endpoints a client that presents its certificate uses instead.
	// [endpointsFor] decides when they apply.
	MTLSEndpointAliases MTLSEndpointAliases `json:"mtls_endpoint_aliases,omitzero"`
	// TLSClientCertificateBoundAccessTokens is RFC 8705 §3.3's
	// tls_client_certificate_bound_access_tokens: the server binds the
	// tokens it issues to the client certificate presented.
	TLSClientCertificateBoundAccessTokens bool `json:"tls_client_certificate_bound_access_tokens,omitempty"`
}

// MTLSEndpointAliases are the mTLS aliases of the endpoints Wayfindr calls
// with client authentication (RFC 8705 §5); an empty one has no alias.
type MTLSEndpointAliases struct {
	Token               string `json:"token_endpoint,omitempty"`
	DeviceAuthorization string `json:"device_authorization_endpoint,omitempty"`
	Registration        string `json:"registration_endpoint,omitempty"`
}

// parseMetadataExtensions reads the [MetadataExtensions] of an
// authorization server metadata document. It is their one parse: a field
// the SDK lacks joins MetadataExtensions rather than being read elsewhere.
func parseMetadataExtensions(doc []byte) (MetadataExtensions, error) {
	var x MetadataExtensions
	err := json.Unmarshal(doc, &x)

	return x, err
}

// DiscoverRequest is one discovery for one connector.
type DiscoverRequest struct {
	// Connector is the connector name, for logs.
	Connector string
	// ResourceURL is the connector's resolved MCP URL, the protected
	// resource.
	ResourceURL string
	// Auth is the connector's [auth] table, with config defaults applied.
	Auth config.Auth
	// Challenge holds the WWW-Authenticate values of an upstream 401 or 403,
	// if there was one.
	Challenge []string
	// Probe, when Challenge is empty, reads the challenge by probing the
	// resource (see [Discoverer.probe]). The challenge of the last probe of
	// the resource is reused until a discovery with Refetch, which probes
	// again.
	Probe bool
	// Refetch skips cached documents, and the cached challenge, and fetches
	// every one again. Callers set it once after an auth failure (design
	// §7.3).
	Refetch bool
	// PRMURL is where a grant's flow found the PRM. When the challenge
	// names no resource_metadata, it is the first PRM candidate, so a
	// refresh finds a PRM at a path only a challenge named (wayfindr-q8y9).
	PRMURL string
	// Client is the connector's HTTP client, which every fetch and probe
	// goes through, so they trust its ca_file and present its client
	// certificate; nil uses the Discoverer's own. The cache is keyed by URL
	// alone.
	Client *http.Client
	// Mask hides the values of the references in the connector's url,
	// which ResourceURL and the documents found from it carry, in logs,
	// errors, and the cache: nothing that holds one is cached
	// (wayfindr-wt2v). See [Broker.urlMask].
	Mask redact.Values
}

// Discovery is the outcome of the discovery chain.
type Discovery struct {
	// Resource is the protected resource (the request's ResourceURL).
	Resource string
	// PRM is the Protected Resource Metadata, or nil when none was found or
	// the authorization_server override skipped it. PRMURL is where it came
	// from.
	PRM    *oauthex.ProtectedResourceMetadata
	PRMURL string
	// Issuer is the authorization server's issuer URL: from PRM, from the
	// authorization_server override, or the MCP origin for a legacy server.
	// It is empty when config supplies the endpoints and neither PRM nor
	// authorization_server names an issuer.
	Issuer string
	// Metadata is the authorization server metadata, or nil when the
	// endpoints come from config or the legacy defaults. MetadataURL is
	// where it came from.
	Metadata    *oauthex.AuthServerMeta
	MetadataURL string
	// Extensions are the fields of the metadata document that Metadata
	// lacks; zero when Metadata is nil.
	Extensions MetadataExtensions
	// Endpoints are the endpoints in effect, and Source says where they
	// came from.
	Endpoints Endpoints
	Source    Source
	// ChallengeScopes is the scope parameter of the Bearer or DPoP
	// challenge, if any (ADR 0007 scope order).
	ChallengeScopes []string
	// Challenge is the WWW-Authenticate challenge discovery read: the
	// request's, or the probe's; nil when there was none. [chooseSender]
	// reads whether it offers DPoP.
	Challenge []string
}

// maxDocument bounds a metadata document, as oauthex does.
const maxDocument = 1 << 20

// redactErr strips URL queries from an error's text for logging.
func redactErr(err error) error { return errors.New(redact.URLQueries(err.Error())) }

// Discoverer runs the ADR 0007 discovery chain and caches documents in the
// store's oauth_metadata table.
type Discoverer struct {
	store  store.Store
	client *http.Client
	log    zerolog.Logger
	now    func() time.Time

	// challenges caches, by resource URL, the challenge the last probe read
	// (nil for an answer without one), next to the documents it leads to.
	// It is in memory: after a restart the first discovery probes again.
	mu         sync.Mutex
	challenges map[string][]string
}

// NewDiscoverer returns a Discoverer that fetches through client, unless a
// request names its own, and caches in st.
func NewDiscoverer(st store.Store, client *http.Client, log zerolog.Logger) *Discoverer {
	return &Discoverer{store: st, client: client, log: log, now: time.Now, challenges: map[string][]string{}}
}

// Discover runs the discovery chain for one connector. Config overrides come
// first: authorization_server skips PRM, and authorization_endpoint with
// token_endpoint skip the authorization server metadata. Cached documents are
// used unless req.Refetch is set.
//
// Errors are [*Error]s: [KindConfig] means the connector belongs in
// auth_error, and a failure to reach a metadata server is [KindUnreachable],
// wrapping the transport error.
func (d *Discoverer) Discover(ctx context.Context, req DiscoverRequest) (Discovery, error) {
	disc, err := d.discover(ctx, req)

	return disc, withMask(err, req.Mask)
}

// discover is Discover, its errors not yet masked.
func (d *Discoverer) discover(ctx context.Context, req DiscoverRequest) (Discovery, error) {
	res, err := url.Parse(req.ResourceURL)

	if err != nil || res.Scheme == "" || res.Host == "" {
		return Discovery{}, fail(CaseInvalidURL, req.Connector,
			fmt.Sprintf("connector url %q is not absolute", redact.URL(req.ResourceURL)), nil)
	}

	out := Discovery{Resource: req.ResourceURL}

	if req.Refetch {
		d.forgetChallenge(req.ResourceURL)
	}

	if req.Probe && len(req.Challenge) == 0 {
		req.Challenge = d.challenge(ctx, req)
	}

	resourceMetadata := ""
	out.Challenge = req.Challenge

	if len(req.Challenge) > 0 {
		challenges, err := oauthex.ParseWWWAuthenticate(req.Challenge)

		if err != nil {
			d.log.Warn().Str("connector", req.Connector).Err(req.Mask.Err(err)).Msg("ignoring malformed www-authenticate")
		}

		for _, c := range challenges {
			if !tokenScheme(c.Scheme) {
				continue
			}

			resourceMetadata = c.Params["resource_metadata"]
			out.ChallengeScopes = strings.Fields(c.Params["scope"])
			break
		}
	}

	a := req.Auth
	// A device code connector has no authorization endpoint: its token and
	// device authorization endpoints are its config endpoints.
	configEndpoints := a.TokenEndpoint != "" && (a.AuthorizationEndpoint != "" || a.DeviceAuthorizationEndpoint != "")

	// Step 1: Protected Resource Metadata, unless authorization_server
	// names the authorization server.
	if a.AuthorizationServer == "" {
		prm, prmURL, err := d.discoverPRM(ctx, req, res, cmp.Or(resourceMetadata, req.PRMURL))

		if err != nil {
			return Discovery{}, err
		}

		if prm != nil {
			if len(prm.AuthorizationServers) == 0 && !configEndpoints {
				return Discovery{}, fail(CaseNoAuthorizationServer, req.Connector,
					"protected resource metadata at "+prmURL+" lists no authorization server", nil)
			}

			out.PRM, out.PRMURL = prm, prmURL

			if len(prm.AuthorizationServers) > 0 {
				out.Issuer = prm.AuthorizationServers[0]
			}
		}
	} else {
		out.Issuer = a.AuthorizationServer
	}

	if configEndpoints {
		out.Endpoints = Endpoints{
			Authorization: a.AuthorizationEndpoint, Token: a.TokenEndpoint,
			DeviceAuthorization: a.DeviceAuthorizationEndpoint,
		}
		out.Source = SourceConfig
		d.logDiscovered(req, out)

		return out, nil
	}

	// Step 3 (legacy, 2025-03-26): no PRM and no override, so the MCP
	// origin is the authorization server.
	legacy := out.Issuer == ""

	if legacy {
		out.Issuer = res.Scheme + "://" + res.Host
	}

	// Step 2: authorization server metadata.
	meta, ext, metaURL, err := d.discoverAS(ctx, req, out.Issuer)

	if err != nil {
		return Discovery{}, err
	}

	switch {
	case meta != nil:
		if err := d.checkIssuer(req, out.Issuer, meta, metaURL); err != nil {
			return Discovery{}, err
		}

		out.Metadata, out.MetadataURL, out.Extensions = meta, metaURL, ext
		out.Endpoints = Endpoints{
			Authorization:       meta.AuthorizationEndpoint,
			Token:               meta.TokenEndpoint,
			Registration:        meta.RegistrationEndpoint,
			DeviceAuthorization: cmp.Or(a.DeviceAuthorizationEndpoint, ext.DeviceAuthorizationEndpoint),
		}
		out.Source = SourceMetadata

		if legacy {
			out.Source = SourceLegacyMetadata
		}
	case legacy:
		out.Endpoints = Endpoints{
			Authorization: out.Issuer + "/authorize",
			Token:         out.Issuer + "/token",
			Registration:  out.Issuer + "/register",
			// The 2025-03-26 defaults have no device endpoint.
			DeviceAuthorization: a.DeviceAuthorizationEndpoint,
		}
		out.Source = SourceLegacyDefaults
	default:
		return Discovery{}, fail(CaseNoMetadata, req.Connector,
			"no authorization server metadata for issuer "+out.Issuer, nil)
	}

	d.logDiscovered(req, out)

	return out, nil
}

// probeTimeout bounds a probe of the resource for its WWW-Authenticate
// challenge.
const probeTimeout = 10 * time.Second

// probeInitialize is the body of the probe POST: a minimal MCP initialize
// request, which a Streamable HTTP server must accept without a session.
const probeInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
	`"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"wayfindr","version":"probe"}}}`

// challenge returns the challenge of the last probe of req's resource, or
// probes it (see [Discoverer.probe]) and caches what it read. A probe that
// could not reach the resource is not cached: the next discovery probes
// again.
func (d *Discoverer) challenge(ctx context.Context, req DiscoverRequest) []string {
	d.mu.Lock()
	ch, ok := d.challenges[req.ResourceURL]
	d.mu.Unlock()

	if ok {
		return ch
	}

	ch, err := d.probe(ctx, req)

	if err != nil {
		d.log.Debug().Str("connector", req.Connector).Err(req.Mask.Err(redactErr(err))).Msg("oauth challenge probe failed")

		return nil
	}

	d.mu.Lock()
	d.challenges[req.ResourceURL] = ch
	d.mu.Unlock()

	return ch
}

// forgetChallenge drops the cached challenge of resource.
func (d *Discoverer) forgetChallenge(resource string) {
	d.mu.Lock()
	delete(d.challenges, resource)
	d.mu.Unlock()
}

// probe returns the WWW-Authenticate challenge of the resource's 401, which
// names the PRM and the scopes to ask for (ADR 0007). Every discovery that
// needs the challenge without a request of its own goes through it. Any
// other answer means no challenge (nil); discovery then probes the
// well-known URLs. The error is a failure to reach the resource.
func (d *Discoverer) probe(ctx context.Context, req DiscoverRequest) ([]string, error) {
	ans, err := probeResource(ctx, d.http(req), req.ResourceURL)

	if err != nil || ans.status != http.StatusUnauthorized {
		return nil, err
	}

	return ans.challenge, nil
}

// ProbeOutcome is what an unauthenticated probe says a connector needs.
type ProbeOutcome string

// Probe outcomes.
const (
	// ProbeOAuth: a 401 with a Bearer challenge, or a success from a
	// resource that publishes Protected Resource Metadata naming an
	// authorization server.
	ProbeOAuth ProbeOutcome = "oauth"
	// ProbeOpen: a success, and no Protected Resource Metadata for the
	// resource, so the connector needs no auth.
	ProbeOpen ProbeOutcome = "open"
	// ProbeUnknown: any other answer; Status says which.
	ProbeUnknown ProbeOutcome = "unknown"
)

// ProbeResult is the answer to [Probe].
type ProbeResult struct {
	Outcome ProbeOutcome
	// Status is the HTTP status the outcome was read from.
	Status int
}

// Probe asks the MCP server at resource, without credentials, whether it
// needs OAuth: the probe discovery uses, read for `wayfindr connector add`
// before the connector exists. The error is a failure to reach it, with
// URL queries redacted.
func Probe(ctx context.Context, client *http.Client, resource string) (ProbeResult, error) {
	ans, err := probeResource(ctx, client, resource)

	if err != nil {
		return ProbeResult{}, redactErr(err)
	}

	res := ProbeResult{Outcome: ProbeUnknown, Status: ans.status}

	switch {
	case ans.status == http.StatusUnauthorized && hasTokenChallenge(ans.challenge):
		res.Outcome = ProbeOAuth
	case isSuccess(ans.status) && publishesPRM(ctx, client, resource):
		// It answers without a token, as Google's MCP servers answer
		// initialize, but its tools need one.
		res.Outcome = ProbeOAuth
	case isSuccess(ans.status):
		res.Outcome = ProbeOpen
	}

	return res, nil
}

// publishesPRM reports whether resource publishes Protected Resource
// Metadata naming an authorization server at a well-known URL, which the MCP
// authorization spec requires of a server that uses OAuth, even one that
// answers some requests without a token. It tries the URLs discovery tries
// without a challenge (ADR 0007), so a PRM it finds is the one discovery
// finds. A failure to fetch either URL counts as no PRM.
func publishesPRM(ctx context.Context, client *http.Client, resource string) bool {
	res, err := url.Parse(resource)

	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	for _, u := range prmCandidates(res, "") {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, u, resource, client)

		if err == nil {
			return len(prm.AuthorizationServers) > 0
		}
	}

	return false
}

func isSuccess(status int) bool { return status >= 200 && status < 300 }

// hasTokenChallenge reports whether challenge holds a Bearer or DPoP
// challenge.
func hasTokenChallenge(challenge []string) bool {
	cs, _ := oauthex.ParseWWWAuthenticate(challenge)

	for _, c := range cs {
		if tokenScheme(c.Scheme) {
			return true
		}
	}

	return false
}

// probeResource sends unauthenticated requests to the resource until one
// answers what it needs. It GETs the resource first, which a 401 or a
// success answers (a legacy SSE endpoint answers it with its stream).
// Otherwise it POSTs an initialize: a Streamable HTTP server with no SSE
// stream answers the GET with 405 (wayfindr-zgp.11), and one with sessions
// answers a GET without one with 400.
//
// A server that needs no token may answer the initialize with a session
// (Mcp-Session-Id); the probe ends it with an unauthenticated DELETE,
// best-effort, so probing leaves nothing open (wayfindr-zgp.24).
func probeResource(ctx context.Context, client *http.Client, resource string) (probeAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	ans, err := probeOnce(ctx, client, http.MethodGet, resource, "", "")

	if err != nil || ans.status == http.StatusUnauthorized || isSuccess(ans.status) {
		return ans, err
	}

	ans, err = probeOnce(ctx, client, http.MethodPost, resource, probeInitialize, "")

	if err == nil && ans.session != "" {
		// Best-effort: the server may not allow DELETE (405), and a
		// failure to end the session doesn't change the probe's answer.
		_, _ = probeOnce(ctx, client, http.MethodDelete, resource, "", ans.session)
	}

	return ans, err
}

// probeAnswer is what the probe reads from a response.
type probeAnswer struct {
	status    int
	challenge []string // WWW-Authenticate values
	session   string   // Mcp-Session-Id
}

// probeOnce sends one unauthenticated probe request, with body as JSON
// unless it is empty, and with session as its Mcp-Session-Id unless it is
// empty. The body is closed unread: an upstream that needs no token may
// answer with a stream.
func probeOnce(ctx context.Context, client *http.Client, method, resource, body, session string) (probeAnswer, error) {
	var r io.Reader

	if body != "" {
		r = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, resource, r)

	if err != nil {
		return probeAnswer{}, err
	}

	req.Header.Set("Accept", "application/json, text/event-stream")

	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}

	resp, err := client.Do(req)

	if err != nil {
		return probeAnswer{}, err
	}

	_ = resp.Body.Close()

	return probeAnswer{
		status:    resp.StatusCode,
		challenge: resp.Header.Values("WWW-Authenticate"),
		session:   resp.Header.Get("Mcp-Session-Id"),
	}, nil
}

// checkIssuer applies RFC 8414 §3.3, up to a terminating "/" (see
// sameIssuer), unless validate_issuer = false, in which case it warns on
// every skip (ADR 0007).
func (d *Discoverer) checkIssuer(req DiscoverRequest, issuer string, meta *oauthex.AuthServerMeta, metaURL string) error {
	if !req.Auth.ValidateIssuer {
		d.log.Warn().Str("connector", req.Connector).Str("issuer", req.Mask.Apply(redact.URL(issuer))).
			Str("metadata_issuer", req.Mask.Apply(redact.URL(meta.Issuer))).Msg("issuer validation skipped")

		return nil
	}

	if !sameIssuer(meta.Issuer, issuer) {
		return fail(CaseIssuerMismatch, req.Connector, fmt.Sprintf("authorization server metadata at %s reports issuer %q, want %q",
			metaURL, meta.Issuer, issuer), nil)
	}

	return nil
}

// sameIssuer reports whether a and b name the same issuer. They may differ
// by a terminating "/", which RFC 8414 §3.1 drops before building the
// metadata URL (as asCandidates does), so both lead to the same document.
func sameIssuer(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

func (d *Discoverer) logDiscovered(req DiscoverRequest, out Discovery) {
	d.log.Debug().Str("connector", req.Connector).Str("source", string(out.Source)).
		Str("prm_url", req.Mask.Apply(redact.URL(out.PRMURL))).Str("issuer", req.Mask.Apply(redact.URL(out.Issuer))).
		Str("metadata_url", req.Mask.Apply(redact.URL(out.MetadataURL))).Msg("oauth discovery done")
}

// prmCandidates lists the PRM URLs in ADR 0007 order: the challenge's
// resource_metadata, then the path-inserted and root well-known URLs.
func prmCandidates(res *url.URL, fromChallenge string) []string {
	origin := res.Scheme + "://" + res.Host
	var out []string
	add := func(u string) {
		for _, have := range out {
			if have == u {
				return
			}
		}

		out = append(out, u)
	}

	if fromChallenge != "" {
		add(fromChallenge)
	}

	if p := strings.TrimSuffix(res.EscapedPath(), "/"); p != "" {
		add(origin + "/.well-known/oauth-protected-resource" + p)
	}

	add(origin + "/.well-known/oauth-protected-resource")

	return out
}

// asCandidates lists the authorization server metadata URLs for issuer in
// ADR 0007 order, or false if issuer is not an absolute URL.
func asCandidates(issuer string) ([]string, bool) {
	u, err := url.Parse(issuer)

	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, false
	}

	origin := u.Scheme + "://" + u.Host
	p := strings.TrimSuffix(u.EscapedPath(), "/")

	if p == "" {
		return []string{
			origin + "/.well-known/oauth-authorization-server",
			origin + "/.well-known/openid-configuration",
		}, true
	}

	return []string{
		origin + "/.well-known/oauth-authorization-server" + p,
		origin + "/.well-known/openid-configuration" + p,
		origin + p + "/.well-known/openid-configuration",
	}, true
}

// discoverPRM returns the first PRM in candidate order, or nil if no
// candidate serves one. Cached documents are tried before any fetch.
func (d *Discoverer) discoverPRM(ctx context.Context, req DiscoverRequest, res *url.URL, fromChallenge string) (*oauthex.ProtectedResourceMetadata, string, error) {
	candidates := prmCandidates(res, fromChallenge)

	if !req.Refetch {
		for _, c := range candidates {
			var prm oauthex.ProtectedResourceMetadata

			if d.cached(ctx, req, c, store.MetadataPRM, &prm) && prm.Resource == req.ResourceURL {
				return &prm, c, nil
			}
		}
	}

	for _, c := range candidates {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, c, req.ResourceURL, d.http(req))

		if err != nil {
			if transportErr(err) {
				return nil, "", fail(transportCase(err), req.Connector, "fetch protected resource metadata", err)
			}

			d.log.Debug().Str("connector", req.Connector).Str("url", req.Mask.Apply(redact.URL(c))).Err(req.Mask.Err(err)).Msg("no protected resource metadata")
			continue
		}

		doc, err := json.Marshal(prm)

		if err != nil {
			return nil, "", fail(CaseStore, req.Connector, "encode protected resource metadata", err)
		}

		d.cache(ctx, req, c, store.MetadataPRM, doc)

		return prm, c, nil
	}

	return nil, "", nil
}

// discoverAS returns the first authorization server metadata in candidate
// order, with its [MetadataExtensions], or nil if no candidate serves one.
// Cached documents are tried before any fetch.
func (d *Discoverer) discoverAS(ctx context.Context, req DiscoverRequest, issuer string) (*oauthex.AuthServerMeta, MetadataExtensions, string, error) {
	candidates, ok := asCandidates(issuer)

	if !ok {
		return nil, MetadataExtensions{}, "", fail(CaseInvalidURL, req.Connector,
			fmt.Sprintf("authorization server %q is not an absolute URL", redact.URL(issuer)), nil)
	}

	if !req.Refetch {
		for _, c := range candidates {
			var doc json.RawMessage

			if !d.cached(ctx, req, c, store.MetadataAS, &doc) {
				continue
			}

			if meta, ext, err := readMeta(doc); err == nil {
				return meta, ext, c, nil
			}
		}
	}

	for _, c := range candidates {
		doc, err := d.fetch(ctx, d.http(req), c)

		if err != nil {
			if transportErr(err) {
				return nil, MetadataExtensions{}, "", fail(transportCase(err), req.Connector, "fetch authorization server metadata", err)
			}

			d.log.Debug().Str("connector", req.Connector).Str("url", req.Mask.Apply(redact.URL(c))).Err(req.Mask.Err(err)).Msg("no authorization server metadata")
			continue
		}

		meta, ext, err := readMeta(doc)

		if err != nil {
			d.log.Debug().Str("connector", req.Connector).Str("url", req.Mask.Apply(redact.URL(c))).Err(req.Mask.Err(err)).Msg("bad authorization server metadata")
			continue
		}

		d.cache(ctx, req, c, store.MetadataAS, doc)

		return meta, ext, c, nil
	}

	return nil, MetadataExtensions{}, "", nil
}

// readMeta decodes an authorization server metadata document and its
// [MetadataExtensions], and checks them (see [validMeta]).
func readMeta(doc []byte) (*oauthex.AuthServerMeta, MetadataExtensions, error) {
	var meta oauthex.AuthServerMeta

	if err := json.Unmarshal(doc, &meta); err != nil {
		return nil, MetadataExtensions{}, err
	}

	ext, err := parseMetadataExtensions(doc)

	if err != nil {
		return nil, MetadataExtensions{}, err
	}

	if err := validMeta(&meta, ext); err != nil {
		return nil, MetadataExtensions{}, err
	}

	return &meta, ext, nil
}

// fetch GETs a metadata document. Authorization server metadata is fetched
// here rather than through oauthex.GetAuthServerMeta, which enforces the
// issuer match and PKCE with no opt-out (design §7.3).
func (d *Discoverer) fetch(ctx context.Context, client *http.Client, u string) ([]byte, error) {
	if err := httpsOrLoopback(u); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)

	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)

	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fail(CaseUnexpectedStatus, "", fmt.Sprintf("%s answered status %d", u, resp.StatusCode), nil)
	}

	return io.ReadAll(io.LimitReader(resp.Body, maxDocument))
}

// validMeta checks that metadata has the endpoints discovery needs, and that
// the endpoints a client calls use https (or a loopback host) so a document
// cannot point the broker at a javascript: or plain-http URL.
func validMeta(m *oauthex.AuthServerMeta, ext MetadataExtensions) error {
	if m.AuthorizationEndpoint == "" || m.TokenEndpoint == "" {
		return errors.New("metadata lacks authorization_endpoint or token_endpoint")
	}

	for name, v := range map[string]string{
		"authorization_endpoint": m.AuthorizationEndpoint,
		"token_endpoint":         m.TokenEndpoint,
		"registration_endpoint":  m.RegistrationEndpoint,
		// The client authenticates there as at the token endpoint.
		"device_authorization_endpoint": ext.DeviceAuthorizationEndpoint,
		// And at the mTLS aliases of the three.
		"mtls_endpoint_aliases.token_endpoint":                ext.MTLSEndpointAliases.Token,
		"mtls_endpoint_aliases.device_authorization_endpoint": ext.MTLSEndpointAliases.DeviceAuthorization,
		"mtls_endpoint_aliases.registration_endpoint":         ext.MTLSEndpointAliases.Registration,
	} {
		if v == "" {
			continue
		}

		if err := httpsOrLoopback(v); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}

	return nil
}

// httpsOrLoopback accepts https URLs, and http URLs on a loopback host.
func httpsOrLoopback(raw string) error {
	u, err := url.Parse(raw)

	if err != nil {
		return err
	}

	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		return nil
	}

	return fmt.Errorf("url %s is neither https nor loopback http", redact.URL(raw))
}

// cached decodes the cached document at u into v, reporting whether there
// was one of the right kind.
func (d *Discoverer) cached(ctx context.Context, req DiscoverRequest, u string, kind store.MetadataKind, v any) bool {
	m, err := d.store.GetMetadata(ctx, u)

	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			d.log.Warn().Str("connector", req.Connector).Str("url", req.Mask.Apply(redact.URL(u))).Err(req.Mask.Err(err)).Msg("read oauth metadata cache")
		}

		return false
	}

	return m.Kind == kind && json.Unmarshal(m.Document, v) == nil
}

// cache stores a fetched document. A failed write only costs a refetch
// later, so it is logged and not returned. A document whose url or text
// holds a value of the connector's url references, such as the protected
// resource metadata of a url with a secret in its path, is not stored: the
// cache is plaintext (wayfindr-wt2v). It is fetched again each time.
func (d *Discoverer) cache(ctx context.Context, req DiscoverRequest, u string, kind store.MetadataKind, doc []byte) {
	if req.Mask.Contains(u) || req.Mask.Contains(string(doc)) {
		return
	}

	err := d.store.PutMetadata(ctx, store.Metadata{URL: u, Kind: kind, Document: doc, FetchedAt: d.now()})

	if err != nil {
		d.log.Warn().Str("connector", req.Connector).Str("url", req.Mask.Apply(redact.URL(u))).Err(req.Mask.Err(err)).Msg("write oauth metadata cache")
	}
}

// http is the client req's fetches and probes go through: the connector's,
// else the Discoverer's own.
func (d *Discoverer) http(req DiscoverRequest) *http.Client {
	if req.Client != nil {
		return req.Client
	}

	return d.client
}

// transportErr reports whether err is a failure to reach the server, as
// opposed to a server that answered without a usable document.
func transportErr(err error) bool {
	var ue *url.Error

	return errors.As(err, &ue)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}

	ip, err := netip.ParseAddr(host)

	return err == nil && ip.IsLoopback()
}
