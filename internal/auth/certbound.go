package auth

import (
	"context"

	"github.com/atomicpages/wayfindr/internal/config"
)

// certBoundHint is what a token_rejected error of connector c adds to its
// detail when the upstream may want a certificate-bound token (RFC 8705
// §3): its protected resource metadata advertises
// tls_client_certificate_bound_access_tokens and c has no client
// certificate, so its tokens can't be bound. The flag says the resource
// supports such tokens, not that it requires them, so nothing is checked
// before a call: this only points at the fix once a token was rejected. It
// is "" otherwise, and when the metadata can't be read; discovery runs from
// cached documents.
func (b *Broker) certBoundHint(ctx context.Context, c config.Connector) string {
	if c.TLS != nil && c.TLS.HasClientCert() {
		return ""
	}

	resource, err := c.URL.Resolve(ctx, b.resolver)

	if err != nil {
		return ""
	}

	hc, err := b.httpFor(ctx, c)

	if err != nil {
		return ""
	}

	disc, err := b.discover.Discover(ctx, DiscoverRequest{
		Connector: c.Name, ResourceURL: resource, Auth: *c.Auth, Client: hc, Mask: b.urlMask(ctx, c),
	})

	if err != nil || disc.PRM == nil || !disc.PRM.TLSClientCertificateBoundAccessTokens {
		return ""
	}

	return "; the resource supports certificate-bound tokens, and the connector has no client certificate: " +
		"if the resource requires them, add cert_file and key_file in [connectors." + c.Name + ".tls]"
}
