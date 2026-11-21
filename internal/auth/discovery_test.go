package auth_test

import (
	"bytes"
	"net/http"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// recorder is the injected HTTP client's transport. It records the path of
// every request discovery makes, which is how the lookup order is observed.
type recorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()

	return http.DefaultTransport.RoundTrip(req)
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.paths
	r.paths = nil

	return p
}

type harness struct {
	d   *auth.Discoverer
	rec *recorder
	log *bytes.Buffer
}

// newHarness builds a Discoverer over a real SQLite store in a temp dir.
func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	return newHarnessWithStore(t, st)
}

func newHarnessWithStore(t *testing.T, st *sqlite.Store) *harness {
	t.Helper()
	rec := &recorder{}
	var buf bytes.Buffer
	d := auth.NewDiscoverer(st, &http.Client{Transport: rec}, zerolog.New(&buf))

	return &harness{d: d, rec: rec, log: &buf}
}

// defaultAuth is an [auth] table with the config defaults applied.
func defaultAuth() config.Auth {
	return config.Auth{Type: config.AuthTypeOAuth, PKCE: true, ValidateIssuer: true, ResourceIndicator: true}
}

// challengeOf returns the WWW-Authenticate values of the resource's 401.
func challengeOf(t *testing.T, s *fakeas.Server) []string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.ResourceURL, nil)

	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)

	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("resource status = %d, want 401", resp.StatusCode)
	}

	return resp.Header.Values("WWW-Authenticate")
}

// summary is the part of a Discovery the lookup-order tests compare.
type summary struct {
	PRMURL      string
	Issuer      string
	MetadataURL string
	Source      auth.Source
	Endpoints   auth.Endpoints
}

func summarize(d auth.Discovery) summary {
	return summary{d.PRMURL, d.Issuer, d.MetadataURL, d.Source, d.Endpoints}
}

func metadataEndpoints(s *fakeas.Server) auth.Endpoints {
	return auth.Endpoints{
		Authorization: s.AuthorizationEndpoint,
		Token:         s.TokenEndpoint,
		Registration:  s.RegistrationEndpoint,
	}
}

func TestDiscoverLookupOrder(t *testing.T) {
	const (
		prmInserted = "/.well-known/oauth-protected-resource/mcp"
		prmRoot     = "/.well-known/oauth-protected-resource"
		oauthRoot   = "/.well-known/oauth-authorization-server"
	)
	tests := []struct {
		name      string
		opts      []fakeas.Option
		challenge bool
		want      func(s *fakeas.Server) summary
		wantPaths []string
	}{
		{
			name: "PRM path-inserted, metadata at root",
			want: func(s *fakeas.Server) summary {
				return summary{s.PRMURL, s.Issuer, s.MetadataURL, auth.SourceMetadata, metadataEndpoints(s)}
			},
			wantPaths: []string{prmInserted, oauthRoot},
		},
		{
			name: "PRM at root after path-inserted misses",
			opts: []fakeas.Option{fakeas.WithPRMLocation(fakeas.PRMRoot)},
			want: func(s *fakeas.Server) summary {
				return summary{s.PRMURL, s.Issuer, s.MetadataURL, auth.SourceMetadata, metadataEndpoints(s)}
			},
			wantPaths: []string{prmInserted, prmRoot, oauthRoot},
		},
		{
			name:      "challenge resource_metadata first",
			opts:      []fakeas.Option{fakeas.WithPRMLocation(fakeas.PRMRoot), fakeas.WithChallengeResourceMetadata()},
			challenge: true,
			want: func(s *fakeas.Server) summary {
				return summary{s.PRMURL, s.Issuer, s.MetadataURL, auth.SourceMetadata, metadataEndpoints(s)}
			},
			wantPaths: []string{prmRoot, oauthRoot},
		},
		{
			name:      "bare challenge falls back to well-known PRM",
			opts:      []fakeas.Option{fakeas.WithPRMLocation(fakeas.PRMRoot)},
			challenge: true,
			want: func(s *fakeas.Server) summary {
				return summary{s.PRMURL, s.Issuer, s.MetadataURL, auth.SourceMetadata, metadataEndpoints(s)}
			},
			wantPaths: []string{prmInserted, prmRoot, oauthRoot},
		},
		{
			name: "root issuer, OIDC after RFC 8414 misses",
			opts: []fakeas.Option{fakeas.WithMetadataLocation(fakeas.OIDCPathInserted)},
			want: func(s *fakeas.Server) summary {
				return summary{s.PRMURL, s.Issuer, s.MetadataURL, auth.SourceMetadata, metadataEndpoints(s)}
			},
			wantPaths: []string{prmInserted, oauthRoot, "/.well-known/openid-configuration"},
		},
		{
			name: "issuer path, RFC 8414 path-inserted",
			opts: []fakeas.Option{fakeas.WithIssuerPath("/tenant1")},
			want: func(s *fakeas.Server) summary {
				return summary{s.PRMURL, s.Issuer, s.MetadataURL, auth.SourceMetadata, metadataEndpoints(s)}
			},
			wantPaths: []string{prmInserted, oauthRoot + "/tenant1"},
		},
		{
			name: "issuer path, OIDC path-inserted",
			opts: []fakeas.Option{fakeas.WithIssuerPath("/tenant1"), fakeas.WithMetadataLocation(fakeas.OIDCPathInserted)},
			want: func(s *fakeas.Server) summary {
				return summary{s.PRMURL, s.Issuer, s.MetadataURL, auth.SourceMetadata, metadataEndpoints(s)}
			},
			wantPaths: []string{prmInserted, oauthRoot + "/tenant1", "/.well-known/openid-configuration/tenant1"},
		},
		{
			name: "issuer path, OIDC path-appended",
			opts: []fakeas.Option{fakeas.WithIssuerPath("/tenant1"), fakeas.WithMetadataLocation(fakeas.OIDCPathAppended)},
			want: func(s *fakeas.Server) summary {
				return summary{s.PRMURL, s.Issuer, s.MetadataURL, auth.SourceMetadata, metadataEndpoints(s)}
			},
			wantPaths: []string{
				prmInserted, oauthRoot + "/tenant1", "/.well-known/openid-configuration/tenant1",
				"/tenant1/.well-known/openid-configuration",
			},
		},
		{
			name: "legacy: no metadata, default endpoints on the MCP origin",
			opts: []fakeas.Option{fakeas.WithoutMetadata()},
			want: func(s *fakeas.Server) summary {
				return summary{"", s.URL, "", auth.SourceLegacyDefaults, auth.Endpoints{
					Authorization: s.URL + "/authorize",
					Token:         s.URL + "/token",
					Registration:  s.URL + "/register",
				}}
			},
			wantPaths: []string{prmInserted, prmRoot, oauthRoot, "/.well-known/openid-configuration"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeas.Start(t, tt.opts...)
			h := newHarness(t)
			req := auth.DiscoverRequest{Connector: "c", ResourceURL: s.ResourceURL, Auth: defaultAuth()}

			if tt.challenge {
				req.Challenge = challengeOf(t, s)
			}

			got, err := h.d.Discover(t.Context(), req)

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
