package auth

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
)

// TestEndpointsFor tests endpointsFor directly: it is the decision table
// of when the mTLS endpoint aliases apply (RFC 8705 §5, design §7.3), a
// pure function whose table is the clearest statement of the rule
// (CODING_STANDARDS: Tests). Its effect at the seam is covered by the
// tls_client_auth flow against the fake authorization server.
func TestEndpointsFor(t *testing.T) {
	plain := Endpoints{ //nolint:gosec // G101: URLs, not credentials
		Authorization: "https://as/authorize", Token: "https://as/token",
		Registration: "https://as/register", DeviceAuthorization: "https://as/device",
	}
	aliased := Endpoints{ //nolint:gosec // G101: URLs, not credentials
		Authorization: "https://as/authorize", Token: "https://mtls.as/token",
		Registration: "https://mtls.as/register", DeviceAuthorization: "https://mtls.as/device",
	}
	aliases := MTLSEndpointAliases{ //nolint:gosec // G101: URLs, not credentials
		Token: "https://mtls.as/token", Registration: "https://mtls.as/register", DeviceAuthorization: "https://mtls.as/device",
	}
	cert := &config.TLS{CertFile: "/c.pem", KeyFile: "/c.key"}
	caOnly := &config.TLS{CAFile: "/ca.pem"}

	tests := []struct {
		name   string
		tls    *config.TLS
		method config.ClientAuthMethod
		bound  bool
		device string // the config's device_authorization_endpoint
		want   Endpoints
	}{
		{name: "tls_client_auth", tls: cert, method: config.AuthMethodTLSClientAuth, want: aliased},
		{name: "self_signed_tls_client_auth", tls: cert, method: config.AuthMethodSelfSignedTLSClientAuth, want: aliased},
		{name: "a certificate and bound tokens", tls: cert, bound: true, want: aliased},
		{name: "a certificate and bound tokens, private_key_jwt", tls: cert, method: config.AuthMethodPrivateKeyJWT, bound: true, want: aliased},
		{name: "a certificate, tokens not bound", tls: cert, want: plain},
		{name: "bound tokens, no certificate", tls: caOnly, bound: true, want: plain},
		{name: "bound tokens, no tls table", bound: true, want: plain},
		{name: "a configured device endpoint stays", tls: cert, method: config.AuthMethodTLSClientAuth, device: "https://as/device",
			want: Endpoints{Authorization: "https://as/authorize", Token: "https://mtls.as/token", //nolint:gosec // G101: URLs, not credentials
				Registration: "https://mtls.as/register", DeviceAuthorization: "https://as/device"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := config.Connector{Name: "x", TLS: tt.tls, Auth: &config.Auth{DeviceAuthorizationEndpoint: tt.device}}
			disc := Discovery{Endpoints: plain, Extensions: MetadataExtensions{
				MTLSEndpointAliases: aliases, TLSClientCertificateBoundAccessTokens: tt.bound,
			}}

			if diff := cmp.Diff(tt.want, endpointsFor(c, tt.method, disc)); diff != "" {
				t.Errorf("endpointsFor (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("no aliases", func(t *testing.T) {
		c := config.Connector{Name: "x", TLS: cert, Auth: &config.Auth{}}

		if diff := cmp.Diff(plain, endpointsFor(c, config.AuthMethodTLSClientAuth, Discovery{Endpoints: plain})); diff != "" {
			t.Errorf("endpointsFor without aliases (-want +got):\n%s", diff)
		}
	})
}
