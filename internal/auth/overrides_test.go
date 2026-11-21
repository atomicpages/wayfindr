package auth_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

func TestDiscoverConfigOverrides(t *testing.T) {
	const (
		prmInserted = "/.well-known/oauth-protected-resource/mcp"
		oauthTenant = "/.well-known/oauth-authorization-server/tenant1"
	)
	configured := auth.Endpoints{ //nolint:gosec // G101: endpoint URLs, not credentials

		Authorization: "https://as.example.com/oauth/authorize",
		Token:         "https://as.example.com/oauth/token",
	}
	tests := []struct {
		name      string
		auth      func(s *fakeas.Server) config.Auth
		want      func(s *fakeas.Server) summary
		wantPaths []string
	}{
		{
			name: "authorization_server skips PRM",
			auth: func(s *fakeas.Server) config.Auth {
				a := defaultAuth()
				a.AuthorizationServer = s.Issuer

				return a
			},
			want: func(s *fakeas.Server) summary {
				return summary{"", s.Issuer, s.MetadataURL, auth.SourceMetadata, metadataEndpoints(s)}
			},
			wantPaths: []string{oauthTenant},
		},
		{
			name: "endpoints skip authorization server metadata",
			auth: func(*fakeas.Server) config.Auth {
				a := defaultAuth()
				a.AuthorizationEndpoint, a.TokenEndpoint = configured.Authorization, configured.Token

				return a
			},
			want: func(s *fakeas.Server) summary {
				return summary{s.PRMURL, s.Issuer, "", auth.SourceConfig, configured}
			},
			wantPaths: []string{prmInserted},
		},
		{
			name: "authorization_server and endpoints skip every fetch",
			auth: func(s *fakeas.Server) config.Auth {
				a := defaultAuth()
				a.AuthorizationServer = s.Issuer
				a.AuthorizationEndpoint, a.TokenEndpoint = configured.Authorization, configured.Token

				return a
			},
			want: func(s *fakeas.Server) summary {
				return summary{"", s.Issuer, "", auth.SourceConfig, configured}
			},
			wantPaths: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeas.Start(t, fakeas.WithIssuerPath("/tenant1"))
			h := newHarness(t)
			got, err := h.d.Discover(t.Context(), auth.DiscoverRequest{Connector: "c", ResourceURL: s.ResourceURL, Auth: tt.auth(s)})

			if err != nil {
				t.Fatalf("Discover: %v", err)
			}

			if diff := cmp.Diff(tt.want(s), summarize(got)); diff != "" {
				t.Errorf("discovery (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff(tt.wantPaths, h.rec.take()); diff != "" {
				t.Errorf("requests (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDiscoverAuthConfigErrors(t *testing.T) {
	tests := []struct {
		name     string
		opts     []fakeas.Option
		auth     func(s *fakeas.Server) config.Auth
		wantCase auth.Case
	}{
		{
			name: "authorization_server without metadata",
			opts: []fakeas.Option{fakeas.WithoutMetadata()},
			auth: func(s *fakeas.Server) config.Auth {
				a := defaultAuth()
				a.AuthorizationServer = s.URL + "/tenant1"

				return a
			},
			wantCase: auth.CaseNoMetadata,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeas.Start(t, tt.opts...)
			h := newHarness(t)
			_, err := h.d.Discover(t.Context(), auth.DiscoverRequest{Connector: "c", ResourceURL: s.ResourceURL, Auth: tt.auth(s)})

			if e, ok := auth.AsError(err); !ok || e.Kind != auth.KindConfig || e.Case != tt.wantCase {
				t.Fatalf("Discover error = %v, want config/%s", err, tt.wantCase)
			}
		})
	}
}
