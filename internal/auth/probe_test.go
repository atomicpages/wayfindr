package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

// TestProbe reads what `wayfindr connector add` needs to know: a Bearer
// 401 means OAuth, a success means no auth unless the resource publishes
// PRM, and anything else is reported with its status.
func TestProbe(t *testing.T) {
	tests := []struct {
		name string
		url  func(t *testing.T) string
		want auth.ProbeResult
	}{
		{
			name: "oauth server",
			url:  func(t *testing.T) string { return fakeas.Start(t).ResourceURL },
			want: auth.ProbeResult{Outcome: auth.ProbeOAuth, Status: http.StatusUnauthorized},
		},
		{
			name: "oauth server that answers GET with 405",
			url:  func(t *testing.T) string { return fakeas.Start(t, fakeas.WithoutGET()).ResourceURL },
			want: auth.ProbeResult{Outcome: auth.ProbeOAuth, Status: http.StatusUnauthorized},
		},
		{
			name: "oauth server that answers initialize without a token",
			url: func(t *testing.T) string {
				return fakeas.Start(t, fakeas.WithAnonymousSession(), fakeas.WithoutGET()).ResourceURL
			},
			want: auth.ProbeResult{Outcome: auth.ProbeOAuth, Status: http.StatusOK},
		},
		{
			name: "oauth server that answers initialize without a token, PRM at the root",
			url: func(t *testing.T) string {
				return fakeas.Start(t, fakeas.WithAnonymousSession(),
					fakeas.WithPRMLocation(fakeas.PRMRoot)).ResourceURL
			},
			want: auth.ProbeResult{Outcome: auth.ProbeOAuth, Status: http.StatusOK},
		},
		{
			name: "server with no auth and another resource's PRM at the root",
			url: func(t *testing.T) string {
				mux := http.NewServeMux()
				mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{}`))
				})
				mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"resource":"https://other.example/mcp",` +
						`"authorization_servers":["https://as.example"]}`))
				})
				s := httptest.NewServer(mux)
				t.Cleanup(s.Close)

				return s.URL + "/mcp"
			},
			want: auth.ProbeResult{Outcome: auth.ProbeOpen, Status: http.StatusOK},
		},
		{
			name: "server with sessions and no auth",
			url: func(t *testing.T) string {
				return fakeupstream.StartStreamable(t, fakeupstream.V20251125).URL
			},
			want: auth.ProbeResult{Outcome: auth.ProbeOpen, Status: http.StatusOK},
		},
		{
			name: "stateless server with no auth",
			url: func(t *testing.T) string {
				return fakeupstream.StartStreamable(t, fakeupstream.V20260728).URL
			},
			want: auth.ProbeResult{Outcome: auth.ProbeOpen, Status: http.StatusOK},
		},
		{
			name: "legacy SSE server with no auth",
			url: func(t *testing.T) string {
				return fakeupstream.StartSSE(t, fakeupstream.V20251125).URL
			},
			want: auth.ProbeResult{Outcome: auth.ProbeOpen, Status: http.StatusOK},
		},
		{
			name: "server error",
			url: func(t *testing.T) string {
				return fakeupstream.StartStreamable(t, fakeupstream.V20251125,
					fakeupstream.WithStreamableStatus(http.StatusBadGateway)).URL
			},
			want: auth.ProbeResult{Outcome: auth.ProbeUnknown, Status: http.StatusBadGateway},
		},
		{
			name: "401 without a Bearer challenge",
			url: func(t *testing.T) string {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
					http.Error(w, "no", http.StatusUnauthorized)
				}))
				t.Cleanup(s.Close)

				return s.URL
			},
			want: auth.ProbeResult{Outcome: auth.ProbeUnknown, Status: http.StatusUnauthorized},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := auth.Probe(t.Context(), &http.Client{}, tt.url(t))

			if err != nil {
				t.Fatalf("Probe: %v", err)
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Probe (-want +got):\n%s", diff)
			}
		})
	}
}

func TestProbeUnreachable(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	u := s.URL
	s.Close()

	_, err := auth.Probe(t.Context(), &http.Client{}, u+"/mcp?key=s3cret")

	if err == nil {
		t.Fatal("Probe of a closed server succeeded, want an error")
	}

	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error %q holds the URL's query", err)
	}
}
