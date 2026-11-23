package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/atomicpages/wayfindr/internal/redact"
)

const (
	// maxArchiveBytes caps a downloaded archive, and maxBinaryBytes the binary
	// extracted from it, so a broken or hostile server cannot fill memory.
	// Release archives are around 15 MiB.
	maxArchiveBytes = 200 << 20
	maxBinaryBytes  = 200 << 20
	// maxChecksumsBytes caps checksums.txt, a few hundred bytes in practice.
	maxChecksumsBytes = 1 << 20
	// maxRedirects matches net/http's own default limit.
	maxRedirects = 10
)

// Source is where releases come from. The zero value reads GitHub with a
// default client.
type Source struct {
	// BaseURL is the releases root; "" means DefaultBaseURL. It is laid out
	// as GitHub lays it out: <base>/latest redirects to <base>/tag/<tag>, and
	// <base>/download/<tag>/<file> serves each release asset.
	BaseURL string
	// Client makes the requests; nil means a client with timeouts that never
	// follows a redirect from https to http.
	Client *http.Client
}

// Latest returns the tag of the newest release, read from where
// <base>/latest redirects. The redirect is not followed. Like the installers,
// this skips the GitHub API and its rate limit, and GitHub's latest already
// leaves out pre-releases and drafts.
func (s Source) Latest(ctx context.Context) (string, error) {
	u := s.base() + "/latest"

	// Copy the client so the caller's keeps its own redirect policy.
	c := *s.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("latest release: %w", err)
	}

	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("latest release: %w", redactErr(err))
	}

	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", fmt.Errorf("latest release: GET %s: want a redirect to the release, got %s", u, resp.Status)
	}

	loc, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("latest release: GET %s: redirect: %w", u, redactErr(err))
	}

	// .../releases/tag/<tag>; GitHub sends .../releases when there is no
	// release yet.
	dir, tag := path.Split(loc.Path)
	if !strings.HasSuffix(dir, "/tag/") {
		return "", fmt.Errorf("latest release: GET %s: redirect to %s names no release tag", u, redact.URL(loc.String()))
	}

	if !isFullSemver(tag) {
		return "", fmt.Errorf("latest release: GET %s: tag %q is not a semantic version", u, tag)
	}

	return tag, nil
}

// Fetch downloads a release asset and returns the Wayfindr binary inside it.
// The archive must be listed in the release's checksums.txt with a matching
// SHA-256, or Fetch returns an error and nothing else. The binary is the
// wayfindr entry of a .tar.gz archive, or wayfindr.exe of a .zip.
func (s Source) Fetch(ctx context.Context, tag, asset string) ([]byte, error) {
	if tag == "" || asset == "" {
		return nil, errors.New("fetch release: tag and asset are required")
	}

	binName, unpack, err := unpacker(asset)
	if err != nil {
		return nil, err
	}

	dir := s.base() + "/download/" + url.PathEscape(tag) + "/"
	c := s.client()

	sums, err := get(ctx, c, dir+"checksums.txt", maxChecksumsBytes)
	if err != nil {
		return nil, err
	}

	want, ok := checksumFor(sums, asset)
	if !ok {
		return nil, fmt.Errorf("%s is not listed in checksums.txt of %s", asset, tag)
	}

	archive, err := get(ctx, c, dir+url.PathEscape(asset), maxArchiveBytes)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, want) {
		return nil, fmt.Errorf("checksum mismatch for %s: expected %s, got %s", asset, strings.ToLower(want), got)
	}

	bin, err := unpack(archive, binName)
	if err != nil {
		return nil, fmt.Errorf("extract %s from %s: %w", binName, asset, err)
	}

	return bin, nil
}

func (s Source) base() string {
	if s.BaseURL == "" {
		return DefaultBaseURL
	}

	return strings.TrimRight(s.BaseURL, "/")
}

func (s Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}

	return defaultClient()
}

// defaultClient bounds each stage of a request, and the whole request,
// download included, at 15 minutes: long enough for a slow but live
// connection to finish a release archive. The caller's ctx can end it
// sooner. It refuses to follow a redirect off https, as curl --proto-redir '=https'
// does in install.sh.
func defaultClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
		Timeout: 15 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}

			if via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect from https to %s", req.URL.Scheme)
			}

			return nil
		},
	}
}

// get downloads u in full, refusing a body larger than limit bytes.
func get(ctx context.Context, c *http.Client, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", redact.URL(u), err)
	}

	resp, err := c.Do(req)
	if err != nil {
		// *url.Error already names the method and URL: the URL redirected to,
		// which for a release asset is a signed one.
		return nil, redactErr(err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", redact.URL(u), resp.Status)
	}

	b, err := readCapped(resp.Body, limit)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", redact.URL(u), err)
	}

	return b, nil
}

// readCapped reads r to the end, failing once it passes limit bytes.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}

	if int64(len(b)) > limit {
		return nil, fmt.Errorf("larger than %d MiB", limit>>20)
	}

	return b, nil
}

// checksumFor finds asset's SHA-256 in checksums.txt, whose lines are
// "<sha256 hex>  <file name>", matching the file name exactly as the
// installers do.
func checksumFor(sums []byte, asset string) (string, bool) {
	for line := range strings.Lines(string(sums)) {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == asset {
			return f[0], true
		}
	}

	return "", false
}

// unpacker picks the binary's name and the archive reader by the asset's
// suffix, as .goreleaser.yaml pairs them.
func unpacker(asset string) (string, func([]byte, string) ([]byte, error), error) {
	switch {
	case strings.HasSuffix(asset, ".tar.gz"):
		return "wayfindr", untarGz, nil
	case strings.HasSuffix(asset, ".zip"):
		return "wayfindr.exe", unzip, nil
	default:
		return "", nil, fmt.Errorf("fetch release: unsupported archive %s: want .tar.gz or .zip", asset)
	}
}

func untarGz(archive []byte, name string) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}

	defer func() { _ = zr.Close() }()

	tr := tar.NewReader(zr)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("not found in archive")
		}

		if err != nil {
			return nil, err
		}

		if h.Typeflag != tar.TypeReg || path.Clean(h.Name) != name {
			continue
		}

		return readBinary(tr)
	}
}

func unzip(archive []byte, name string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}

	for _, f := range zr.File {
		if !f.Mode().IsRegular() || path.Clean(f.Name) != name {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return nil, err
		}

		b, err := readBinary(rc)
		_ = rc.Close()

		return b, err
	}

	return nil, errors.New("not found in archive")
}

func readBinary(r io.Reader) ([]byte, error) {
	b, err := readCapped(r, maxBinaryBytes)
	if err != nil {
		return nil, err
	}

	if len(b) == 0 {
		return nil, errors.New("empty file in archive")
	}

	return b, nil
}

// redactErr masks the URL in a *url.Error, which net/http returns for a
// failed request and url.Parse for a bad Location: after a redirect it is
// the URL redirected to, whose query carries GitHub's signed download token
// (design §9). The cause stays reachable with errors.Is and errors.As.
func redactErr(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}

	return &url.Error{Op: ue.Op, URL: redact.URL(ue.URL), Err: ue.Err}
}
