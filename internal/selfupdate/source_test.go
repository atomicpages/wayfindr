package selfupdate_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/selfupdate"
)

// release is one release on the fake GitHub: its assets by file name.
// checksums.txt is served from the map like any other asset.
type release map[string][]byte

// fakeGitHub serves a releases root laid out like GitHub's download URLs:
// /releases/download/<tag>/<file> serves the release's assets.
func fakeGitHub(t *testing.T, releases map[string]release) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases/download/{tag}/{file}", func(w http.ResponseWriter, r *http.Request) {
		b, ok := releases[r.PathValue("tag")][r.PathValue("file")]
		if !ok {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write(b)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

func source(srv *httptest.Server) selfupdate.Source {
	return selfupdate.Source{BaseURL: srv.URL + "/releases", Client: srv.Client()}
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)

	for _, name := range sortedKeys(files) {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func zipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	for _, name := range sortedKeys(files) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := w.Write([]byte(files[name])); err != nil {
			t.Fatal(err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

func sha(b []byte) string {
	s := sha256.Sum256(b)

	return hex.EncodeToString(s[:])
}

// checksums builds checksums.txt in goreleaser's "<sha256>  <name>" format.
func checksums(assets map[string][]byte) []byte {
	var b strings.Builder
	for name, body := range assets {
		fmt.Fprintf(&b, "%s  %s\n", sha(body), name)
	}

	return []byte(b.String())
}

func TestLatest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		latestTo func(srvURL string) string
		want     string
		wantErr  string
	}{
		{
			name:     "absolute redirect to a tag",
			latestTo: func(u string) string { return u + "/releases/tag/v0.2.0" },
			want:     "v0.2.0",
		},
		{
			name:     "github.com style redirect",
			latestTo: func(string) string { return "https://github.com/atomicpages/wayfindr/releases/tag/v1.10.3" },
			want:     "v1.10.3",
		},
		{
			name:     "relative redirect",
			latestTo: func(string) string { return "/releases/tag/v0.3.0" },
			want:     "v0.3.0",
		},
		{
			name:     "no redirect",
			latestTo: func(string) string { return "" },
			wantErr:  "want a redirect",
		},
		{
			name:     "tag is not semver",
			latestTo: func(u string) string { return u + "/releases/tag/nightly" },
			wantErr:  "not a semantic version",
		},
		{
			name:     "tag is semver shorthand",
			latestTo: func(u string) string { return u + "/releases/tag/v1.2" },
			wantErr:  "not a semantic version",
		},
		{
			name:     "redirect without a tag",
			latestTo: func(u string) string { return u + "/releases" },
			wantErr:  "names no release tag",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The redirect target is built from the request's host, the
			// server's own address.
			mux := http.NewServeMux()
			mux.HandleFunc("GET /releases/latest", func(w http.ResponseWriter, r *http.Request) {
				target := tt.latestTo("http://" + r.Host)
				if target == "" {
					_, _ = fmt.Fprint(w, "ok")

					return
				}

				http.Redirect(w, r, target, http.StatusFound)
			})

			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			got, err := source(srv).Latest(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Latest() error = %v, want one containing %q", err, tt.wantErr)
				}

				if !strings.Contains(err.Error(), srv.URL+"/releases/latest") {
					t.Errorf("Latest() error = %v, want it to name the URL", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("Latest() error = %v", err)
			}

			if got != tt.want {
				t.Errorf("Latest() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLatestServerDown(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()

	_, err := selfupdate.Source{BaseURL: u}.Latest(context.Background())
	if err == nil || !strings.Contains(err.Error(), u+"/latest") {
		t.Fatalf("Latest() error = %v, want one naming %s/latest", err, u)
	}
}

func TestFetch(t *testing.T) {
	t.Parallel()

	const bin = "\x7fELF new wayfindr"

	unix := tarGz(t, map[string]string{"LICENSE": "MIT", "README.md": "# wayfindr", "wayfindr": bin})
	windows := zipArchive(t, map[string]string{"LICENSE": "MIT", "README.md": "# wayfindr", "wayfindr.exe": "MZ" + bin})
	assets := map[string][]byte{
		"wayfindr-linux-amd64.tar.gz": unix,
		"wayfindr-windows-arm64.zip":  windows,
	}
	rel := release{"checksums.txt": checksums(assets)}

	for name, b := range assets {
		rel[name] = b
	}

	srv := fakeGitHub(t, map[string]release{"v0.2.0": rel})

	tests := []struct {
		asset string
		want  string
	}{
		{asset: "wayfindr-linux-amd64.tar.gz", want: bin},
		{asset: "wayfindr-windows-arm64.zip", want: "MZ" + bin},
	}

	for _, tt := range tests {
		t.Run(tt.asset, func(t *testing.T) {
			t.Parallel()

			got, err := source(srv).Fetch(context.Background(), "v0.2.0", tt.asset)
			if err != nil {
				t.Fatalf("Fetch() error = %v", err)
			}

			if diff := cmp.Diff(tt.want, string(got)); diff != "" {
				t.Errorf("Fetch() binary mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFetchRejects(t *testing.T) {
	t.Parallel()

	const asset = "wayfindr-darwin-arm64.tar.gz"

	good := tarGz(t, map[string]string{"LICENSE": "MIT", "wayfindr": "binary"})
	noBinary := tarGz(t, map[string]string{"LICENSE": "MIT", "README.md": "# wayfindr"})
	tampered := tarGz(t, map[string]string{"wayfindr": "evil"})

	releases := map[string]release{
		// The archive was swapped after checksums.txt was written.
		"v1.0.0": {
			asset:           tampered,
			"checksums.txt": checksums(map[string][]byte{asset: good}),
		},
		// checksums.txt lists only another platform's archive.
		"v1.0.1": {
			asset:           good,
			"checksums.txt": checksums(map[string][]byte{"wayfindr-linux-amd64.tar.gz": good}),
		},
		// The archive matches its checksum but holds no wayfindr entry.
		"v1.0.2": {
			asset:           noBinary,
			"checksums.txt": checksums(map[string][]byte{asset: noBinary}),
		},
		// Listed, but the archive itself is missing.
		"v1.0.3": {
			"checksums.txt": checksums(map[string][]byte{asset: good}),
		},
	}

	srv := fakeGitHub(t, releases)

	tests := []struct {
		name    string
		tag     string
		asset   string
		wantErr []string
	}{
		{
			name:    "checksum mismatch",
			tag:     "v1.0.0",
			asset:   asset,
			wantErr: []string{"checksum mismatch for " + asset, "expected " + sha(good), "got " + sha(tampered)},
		},
		{
			name:    "asset not in checksums.txt",
			tag:     "v1.0.1",
			asset:   asset,
			wantErr: []string{asset + " is not listed in checksums.txt"},
		},
		{
			name:    "no binary in archive",
			tag:     "v1.0.2",
			asset:   asset,
			wantErr: []string{"extract wayfindr from " + asset, "not found"},
		},
		{
			name:    "archive missing",
			tag:     "v1.0.3",
			asset:   asset,
			wantErr: []string{srv.URL + "/releases/download/v1.0.3/" + asset, "404"},
		},
		{
			name:    "release missing",
			tag:     "v9.9.9",
			asset:   asset,
			wantErr: []string{srv.URL + "/releases/download/v9.9.9/checksums.txt", "404"},
		},
		{
			name:    "unsupported archive",
			tag:     "v1.0.0",
			asset:   "wayfindr-linux-amd64.rpm",
			wantErr: []string{"unsupported archive"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := source(srv).Fetch(context.Background(), tt.tag, tt.asset)
			if err == nil {
				t.Fatalf("Fetch() = %q, want an error", got)
			}

			if got != nil {
				t.Errorf("Fetch() returned %d bytes along with the error", len(got))
			}

			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Fetch() error = %v, want it to contain %q", err, want)
				}
			}
		})
	}
}

// A transport error after a redirect names the URL redirected to, which for
// a GitHub release asset carries a signed token in its query. The error
// keeps the URL's path but not its query (design §9).
func TestFetchErrorRedactsTheRedirectedURL(t *testing.T) {
	t.Parallel()

	// A server that is already gone: the request redirected to it fails.
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()

	signed := goneURL + "/signed/asset?X-Amz-Signature=0123456789abcdef&X-Amz-Credential=AKIA"

	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases/download/{tag}/{file}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, signed, http.StatusFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, err := source(srv).Fetch(context.Background(), "v0.2.0", "wayfindr-linux-amd64.tar.gz")
	if err == nil {
		t.Fatal("Fetch() succeeded, want a transport error")
	}

	if strings.Contains(err.Error(), "X-Amz") || strings.Contains(err.Error(), "0123456789abcdef") {
		t.Errorf("Fetch() error = %v, want the query redacted", err)
	}

	if !strings.Contains(err.Error(), goneURL+"/signed/asset") {
		t.Errorf("Fetch() error = %v, want it to name the URL redirected to, without its query", err)
	}
}
